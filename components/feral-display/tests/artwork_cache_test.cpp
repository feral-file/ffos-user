#include "ff/artwork_cache.hpp"

#include <fcntl.h>
#include <sys/file.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <unistd.h>

#include <cerrno>
#include <csignal>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iostream>

// Keep the retention tests independent of the build machine's free space.
extern "C" int __wrap_statvfs(const char*, struct statvfs* st) {
  *st = {};
  st->f_frsize = 4096;
  st->f_blocks = 8U << 20;
  st->f_bavail = 4U << 20;
  return 0;
}

namespace fs = std::filesystem;
namespace {
void check(bool condition, const char* description) {
  if (!condition) throw std::runtime_error(description);
}
template <class Predicate>
void wait_for(Predicate pred) {
  for (int i = 0; i < 500; ++i) {
    if (pred()) return;
    usleep(10000);
  }
  throw std::runtime_error("timed out waiting for cache thread");
}
// A local origin behind the same one-stream descriptor protocol as ff-egress.
// No tests open Internet sockets.
int serve() {
  int streams[2];
  char token = 0;
  if (read(3, &token, 1) != 1 || socketpair(AF_UNIX, SOCK_STREAM, 0, streams)) return 1;
  alignas(cmsghdr) char rights[CMSG_SPACE(sizeof(int))]{};
  iovec io{&token, 1};
  msghdr msg{};
  msg.msg_iov = &io;
  msg.msg_iovlen = 1;
  msg.msg_control = rights;
  msg.msg_controllen = sizeof(rights);
  auto* c = CMSG_FIRSTHDR(&msg);
  c->cmsg_level = SOL_SOCKET;
  c->cmsg_type = SCM_RIGHTS;
  c->cmsg_len = CMSG_LEN(sizeof(int));
  memcpy(CMSG_DATA(c), &streams[1], sizeof(int));
  if (sendmsg(3, &msg, 0) != 1) return 1;
  close(streams[1]);
  std::string request;
  char buffer[4096];
  while (request.find("\r\n\r\n") == request.npos) {
    auto n = read(streams[0], buffer, sizeof(buffer));
    if (n <= 0) return 1;
    request.append(buffer, n);
  }
  auto start = request.find("http://fixture.test/");
  if (start == request.npos) return 1;
  start += strlen("http://fixture.test/");
  auto name = request.substr(start, request.find(' ', start) - start);
  std::ofstream(fs::path(getenv("HOME")) / name) << getpid();
  std::string response = "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Type: ";
  response += name == "playlist" ? "application/vnd.apple.mpegurl" : "video/mp4";
  response += "\r\nCache-Control: ";
  response += name == "no-store" ? "no-store, max-age=3600" : "max-age=3600";
  if (name == "vary") response += "\r\nVary: Cookie";
  if (name == "stale") response += "\r\nAge: 4000";
  response += "\r\n\r\n";
  response += name == "stall" || name == "truncated" ? "m" : "media";
  send(streams[0], response.data(), response.size(), MSG_NOSIGNAL);
  if (name == "truncated") shutdown(streams[0], SHUT_WR);
  for (;;) pause();  // Host retires its broker after publishing or rejecting the body.
}
}  // namespace
int main(int argc, char** argv) {
  if (argc == 2 && std::string_view(argv[1]) == "serve") return serve();
  char tmp[] = "/tmp/ff-player-cache-XXXXXX";
  if (!mkdtemp(tmp)) return 1;
  fs::path home = tmp, root = home / ".cache/player-artworks";
  setenv("HOME", tmp, 1);
  int result = 0;
  try {
    ff::ArtworkCache cache(fs::canonical("/proc/self/exe").string());
    for (auto local : {"/local/art", "./art", "file:///local/art", "fd://3", "data:image/png,1",
                       "ff-render://bundle/x", "http://127.0.0.1:8082/blobs/a", "https://localhost/bundle", "http://[::1]/art"}) {
      check(!ff::ArtworkCache::media_request(local, "", false), "local media was cacheable");
      check(ff::ArtworkCache::browser_key(local, false).empty(), "local browser cache key");
    }
    auto dir = root / std::string(64, 'b');
    fs::create_directories(dir);
    std::ofstream(dir / "asset") << "keep";
    int lease = open(dir.c_str(), O_RDONLY | O_DIRECTORY);
    check(lease >= 0 && !flock(lease, LOCK_EX), "browser lease");
    cache.maintain({{"maxAgeDays", 30}, {"maxBytes", 1}});
    wait_for([&] { return fs::exists(root / "media"); });
    struct stat mode{};
    check(stat((root / "media").c_str(), &mode) == 0 && (mode.st_mode & 0077) == 0,
          "private media directory");
    check(fs::exists(dir / "asset"), "active browser evicted");
    close(lease);
    cache.maintain({{"maxAgeDays", 30}, {"maxBytes", 2}});
    wait_for([&] { return !fs::exists(dir); });
    cache.maintain({{"maxAgeDays", 30}, {"maxBytes", 1024}});
    auto play = [&](const char* name) {
      auto media = *ff::ArtworkCache::media_request(std::string("http://fixture.test/") + name,
                                                    "test", false);
      cache.warm(name, media);
      pid_t pid = -1;
      wait_for([&] { return bool(std::ifstream(home / name) >> pid); });
      if (std::string_view(name) != "stall")
        wait_for([&] { return kill(pid, 0) < 0 && errno == ESRCH; });
      return media.key;
    };
    auto stalled = play("stall");
    wait_for([&] { return fs::exists(root / "media/.partial"); });
    cache.stop_warming();
    for (auto name : {"no-store", "vary", "stale", "playlist", "truncated"}) {
      check(!fs::exists(root / "media" / play(name)), "unusable capture published");
    }
    check(!fs::exists(root / "media" / stalled), "cancelled transfer published");
    int writer = open(root.c_str(), O_RDONLY | O_DIRECTORY);
    check(writer >= 0 && !flock(writer, LOCK_EX), "contention fixture");
    std::jthread release([writer] {
      std::this_thread::sleep_for(std::chrono::milliseconds(200));
      close(writer);
    });
    auto key = play("valid");
    int fd = ff::ArtworkCache::open_media(key, 30);
    char bytes[5];
    check(fd >= 0 && read(fd, bytes, 5) == 5 && std::string(bytes, 5) == "media",
          "complete media handoff");
    close(fd);
    auto old = time(nullptr) - 31 * 86400;
    timespec times[] = {{old, 0}, {time(nullptr) + 3600, 0}};
    check(!utimensat(AT_FDCWD, (root / "media" / key).c_str(), times, 0), "age fixture");
    check(ff::ArtworkCache::open_media(key, 30) < 0, "expired by age");
    cache.maintain({{"maxAgeDays", 29}, {"maxBytes", 1024}});
    wait_for([&] { return !fs::exists(root / "media" / key); });
    check(!fs::exists(root / "media/.partial"), "abandoned partial");
  } catch (const std::exception& e) {
    std::cerr << e.what() << '\n';
    result = 1;
  }
  fs::remove_all(home);
  return result;
}
