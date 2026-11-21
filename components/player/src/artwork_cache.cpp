#include "ff/artwork_cache.hpp"

#include <curl/curl.h>
#include <fcntl.h>
#include <glib.h>
#include <poll.h>
#include <signal.h>
#include <spawn.h>
#include <sys/file.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/statvfs.h>
#include <sys/wait.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <charconv>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <filesystem>
#include <map>
#include <memory>
#include <sstream>
#include <utility>
#include <vector>

extern char** environ;
namespace ff {
namespace {
std::string hash(const std::string& identity) {
  auto digest = g_compute_checksum_for_string(G_CHECKSUM_SHA256, identity.data(), identity.size());
  std::string key = digest ? digest : "";
  g_free(digest);
  return key;
}
bool valid_key(const std::string& s) {
  return s.size() == 64 && s.find_first_not_of("0123456789abcdef") == s.npos;
}
std::string media_path(const std::string& key) {
  const char* home = getenv("HOME");
  if (!home || !*home || !valid_key(key)) return {};
  return std::string(home) + "/.cache/player-artworks/media/" + key;
}
bool http_url(const std::string& url) {
  return url.starts_with("https://") || url.starts_with("http://");
}
}  // namespace
std::string ArtworkCache::browser_key(const std::string& url, bool lan) {
  return http_url(url) ? hash(std::string(lan ? "lan:" : "public:") + url) : "";
}
std::optional<ArtworkCache::Media> ArtworkCache::media_request(const std::string& url,
                                                               const std::string& user_agent,
                                                               bool lan) {
  if (!http_url(url)) return std::nullopt;
  auto key = hash(Json::array({url, user_agent, lan}).dump());
  if (key.empty()) return std::nullopt;
  return Media{url, user_agent, key, lan};
}
void ArtworkCache::discard_media(const std::string& key) { unlink(media_path(key).c_str()); }
int ArtworkCache::open_media(const std::string& key, int64_t max_age_days) {
  int fd = open(media_path(key).c_str(), O_RDONLY | O_NONBLOCK | O_CLOEXEC | O_NOFOLLOW);
  if (fd < 0) return -1;
  struct stat info{};
  auto now = std::time(nullptr);
  if (fstat(fd, &info) != 0 || !S_ISREG(info.st_mode) || info.st_size <= 0 ||
      info.st_mtime <= now || now - info.st_atime >= max_age_days * 86400) {
    close(fd);
    return -1;
  }
  const timespec times[] = {{0, UTIME_NOW}, {0, UTIME_OMIT}};
  futimens(fd, times);
  return fd;
}

namespace {
namespace fs = std::filesystem;
struct Fd {
  explicit Fd(int value) : value(value) {}
  ~Fd() {
    if (value >= 0) close(value);
  }
  Fd(const Fd&) = delete;
  Fd& operator=(const Fd&) = delete;
  int value;
};

int64_t available(const fs::path& root) {
  struct statvfs st{};
  if (statvfs(root.c_str(), &st))
    throw std::system_error(errno, std::generic_category(), "cache filesystem space");
  int64_t block = st.f_frsize;
  return block * static_cast<int64_t>(st.f_bavail) -
         std::max<int64_t>(5LL << 30, block * static_cast<int64_t>(st.f_blocks) / 10);
}

struct Entry {
  fs::path path;
  int64_t size;
  time_t used;
  bool browser, expired;
};

bool prune(const fs::path& root, const Json& policy, int64_t reserve) {
  auto now = time(nullptr);
  std::vector<Entry> entries;
  int64_t total = 0;
  for (bool browser : {false, true}) {
    for (const auto& file : fs::directory_iterator(browser ? root : root / "media")) {
      if (!valid_key(file.path().filename())) continue;
      struct stat st{};
      if (lstat(file.path().c_str(), &st)) continue;
      Entry entry{file.path(), st.st_size, st.st_atime, browser, st.st_mtime <= now};
      if (browser) {
        if (!S_ISDIR(st.st_mode)) continue;
        entry.used = st.st_mtime;
        entry.size = 0;
        entry.expired = false;
        for (const auto& child : fs::recursive_directory_iterator(file.path()))
          if (fs::is_regular_file(child.symlink_status())) entry.size += child.file_size();
      } else if (!S_ISREG(st.st_mode))
        continue;
      total += entry.size;
      entries.push_back(entry);
    }
  }
  std::sort(entries.begin(), entries.end(), [](const Entry& a, const Entry& b) {
    return a.expired != b.expired ? a.expired : a.used < b.used;
  });
  auto limit = std::min(policy.at("maxBytes").get<int64_t>(), total + available(root)) - reserve;
  auto cutoff = now - 86400 * policy.at("maxAgeDays").get<int64_t>();
  for (const auto& e : entries) {
    bool evict = e.expired || e.used <= cutoff || total > limit;
    if (e.browser) {
      if (!evict && now - e.used < 3600) continue;
      Fd dir(open(e.path.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
      if (dir.value < 0) continue;
      if (flock(dir.value, LOCK_EX | LOCK_NB)) {
        if (errno == EWOULDBLOCK && now - e.used >= 3600) futimens(dir.value, nullptr);
        continue;
      }
      struct stat st{};
      if (!evict || fstat(dir.value, &st) || st.st_mtime != e.used) continue;
      fs::remove_all(e.path);
    } else if (!evict || !fs::remove(e.path))
      continue;
    total -= e.size;
  }
  return total <= limit;
}

// Ask the same restricted HTTP proxy used by the media sandbox for one
// connected stream. libcurl must never open an origin socket in the host.
class Proxy {
 public:
  Proxy(const std::string& executable, bool lan) {
    int pair[2];
    if (socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, pair)) return;
    control.value = pair[0];
    Fd child(pair[1]);
    posix_spawn_file_actions_t actions;
    posix_spawn_file_actions_init(&actions);
    posix_spawn_file_actions_adddup2(&actions, child.value, 3);
    char* args[] = {const_cast<char*>(executable.c_str()),
                    const_cast<char*>(lan ? "serve-lan" : "serve"), nullptr};
    int rc = posix_spawn(&pid, executable.c_str(), &actions, nullptr, args, environ);
    posix_spawn_file_actions_destroy(&actions);
    if (rc) {
      pid = -1;
      return;
    }
    char token = 1;
    pollfd ready{control.value, POLLIN, 0};
    if (send(control.value, &token, 1, MSG_NOSIGNAL) != 1 || poll(&ready, 1, 2000) != 1) return;
    alignas(cmsghdr) char rights[CMSG_SPACE(sizeof(int))]{};
    iovec data{&token, 1};
    msghdr msg{};
    msg.msg_iov = &data;
    msg.msg_iovlen = 1;
    msg.msg_control = rights;
    msg.msg_controllen = sizeof(rights);
    if (recvmsg(control.value, &msg, MSG_CMSG_CLOEXEC) != 1) return;
    auto* c = CMSG_FIRSTHDR(&msg);
    if (c && c->cmsg_level == SOL_SOCKET && c->cmsg_type == SCM_RIGHTS &&
        c->cmsg_len == CMSG_LEN(sizeof(int)))
      memcpy(&stream.value, CMSG_DATA(c), sizeof(int));
  }
  ~Proxy() {
    if (pid > 0) {
      kill(pid, SIGKILL);
      while (waitpid(pid, nullptr, 0) < 0 && errno == EINTR) {
      }
    }
  }
  Fd stream{-1};

 private:
  Fd control{-1};
  pid_t pid = -1;
};
std::string trim(std::string_view text) {
  auto a = text.find_first_not_of(" \t\r\n");
  if (a == text.npos) return {};
  return std::string(text.substr(a, text.find_last_not_of(" \t\r\n") - a + 1));
}
std::string lower(std::string text) {
  for (char& c : text) c = g_ascii_tolower(c);
  return text;
}
int64_t decimal(std::string_view s) {
  int64_t n = -1;
  auto [p, ec] = std::from_chars(s.data(), s.data() + s.size(), n);
  return ec == std::errc{} && p == s.data() + s.size() && n >= 0 ? n : -1;
}
using Headers = std::map<std::string, std::string>;
time_t expiry(Headers& h, time_t now) {
  static const std::vector<std::string> types = {
      "image/jpeg", "image/png", "image/gif",  "image/webp",      "image/avif",
      "image/bmp",  "video/mp4", "video/webm", "video/quicktime", "audio/mpeg",
      "audio/mp4",  "audio/ogg", "audio/flac", "audio/wav"};
  auto type = lower(trim(h["content-type"].substr(0, h["content-type"].find(';'))));
  if (std::find(types.begin(), types.end(), type) == types.end() || h.contains("set-cookie") ||
      h.contains("content-range") ||
      (h.contains("content-encoding") && lower(h["content-encoding"]) != "identity"))
    return 0;
  int64_t lifetime = -1;
  for (auto name : {"vary", "cache-control"}) {
    std::istringstream fields(lower(h[name]));
    std::string field;
    while (std::getline(fields, field, ',')) {
      field = trim(field);
      if (std::string_view(name) == "vary") {
        if (!field.empty() && field != "user-agent" && field != "origin" &&
            field != "accept-encoding")
          return 0;
      } else {
        auto sep = field.find('=');
        auto key = trim(field.substr(0, sep));
        if (key == "private" || key == "no-store" || key == "no-cache") return 0;
        if (key == "max-age") {
          if (lifetime >= 0 || sep == field.npos) return 0;
          auto value = trim(field.substr(sep + 1));
          if (value.size() >= 2 && value.front() == '"' && value.back() == '"')
            value = value.substr(1, value.size() - 2);
          lifetime = decimal(value);
          if (lifetime <= 0 || lifetime > 315360000) return 0;
        }
      }
    }
  }
  auto date = curl_getdate(h["date"].c_str(), nullptr);
  if (date < 0 || date > now) date = now;
  auto end = lifetime < 0 ? curl_getdate(h["expires"].c_str(), nullptr) : date + lifetime;
  auto age = h.contains("age") ? decimal(h["age"]) : 0;
  if (age < 0 || age > 315360000 || end <= date) return 0;
  return std::min(end, now + (end - date) - age);
}

struct Transfer {
  const fs::path& root;
  const Json& policy;
  const std::atomic<uint64_t>& generation;
  uint64_t token;
  CURL* curl;
  Headers headers;
  Fd output{-1};
  int64_t length = 0, written = 0, next_check = 0;
  time_t expires = 0;

  size_t write(char* data, size_t count) {
    if (generation != token) return 0;
    if (output.value < 0) {
      long status = 0;
      curl_easy_getinfo(curl, CURLINFO_RESPONSE_CODE, &status);
      expires = expiry(headers, time(nullptr));
      length = decimal(headers["content-length"]);
      if (status != 200 || expires <= time(nullptr) || length <= 0 ||
          length > policy.at("maxBytes").get<int64_t>() || !prune(root, policy, length))
        return 0;
      output.value =
          open((root / "media/.partial").c_str(), O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0600);
      if (output.value < 0) return 0;
    }
    if (static_cast<int64_t>(count) > length - written) return 0;
    if (written >= next_check) {
      if (available(root) < length - written) return 0;
      next_check = written + (1 << 20);
    }
    if (::write(output.value, data, count) != static_cast<ssize_t>(count)) return 0;
    written += count;
    return count;
  }
};

void fill(const fs::path& root, const std::string& egress, const ArtworkCache::Media& media,
          const Json& policy, const std::atomic<uint64_t>& generation, uint64_t token) {
  if (!http_url(media.url) || !valid_key(media.key)) return;
  Fd existing(ArtworkCache::open_media(media.key, policy.at("maxAgeDays").get<int64_t>()));
  if (existing.value >= 0) return;
  auto handle =
      std::unique_ptr<CURL, decltype(&curl_easy_cleanup)>(curl_easy_init(), curl_easy_cleanup);
  if (!handle) return;
  Proxy proxy(egress, media.lan);
  if (proxy.stream.value < 0) return;
  auto* c = handle.get();
  Transfer t{root, policy, generation, token, c, {}};
  curl_easy_setopt(c, CURLOPT_URL, media.url.c_str());
  curl_easy_setopt(c, CURLOPT_DISALLOW_USERNAME_IN_URL, 1L);
  curl_easy_setopt(c, CURLOPT_PROTOCOLS_STR, "http,https");
  curl_easy_setopt(c, CURLOPT_PROXY, "http://127.0.0.1:1");
  curl_easy_setopt(c, CURLOPT_NOPROXY, "");
  curl_easy_setopt(c, CURLOPT_OPENSOCKETDATA, &proxy);
  curl_easy_setopt(
      c, CURLOPT_OPENSOCKETFUNCTION,
      +[](void* p, curlsocktype, struct curl_sockaddr*) -> curl_socket_t {
        return std::exchange(static_cast<Proxy*>(p)->stream.value, -1);
      });
  curl_easy_setopt(
      c, CURLOPT_SOCKOPTFUNCTION,
      +[](void*, curl_socket_t, curlsocktype) { return int(CURL_SOCKOPT_ALREADY_CONNECTED); });
  curl_easy_setopt(c, CURLOPT_SUPPRESS_CONNECT_HEADERS, 1L);
  curl_easy_setopt(c, CURLOPT_USERAGENT, media.user_agent.c_str());
  curl_easy_setopt(c, CURLOPT_ACCEPT_ENCODING, "identity");
  curl_easy_setopt(c, CURLOPT_NOSIGNAL, 1L);
  curl_easy_setopt(c, CURLOPT_CONNECTTIMEOUT, 15L);
  curl_easy_setopt(c, CURLOPT_TIMEOUT, 1800L);
  curl_easy_setopt(c, CURLOPT_MAX_RECV_SPEED_LARGE, curl_off_t(2 << 20));
  curl_easy_setopt(c, CURLOPT_NOPROGRESS, 0L);
  curl_easy_setopt(c, CURLOPT_XFERINFODATA, &t);
  curl_easy_setopt(
      c, CURLOPT_XFERINFOFUNCTION, +[](void* p, curl_off_t, curl_off_t, curl_off_t, curl_off_t) {
        auto& t = *static_cast<Transfer*>(p);
        return int(t.generation != t.token);
      });
  curl_easy_setopt(c, CURLOPT_HEADERDATA, &t);
  curl_easy_setopt(
      c, CURLOPT_HEADERFUNCTION, +[](char* data, size_t a, size_t b, void* p) -> size_t {
        try {
          std::string_view line(data, a * b);
          auto& h = static_cast<Transfer*>(p)->headers;
          if (line.starts_with("HTTP/"))
            h.clear();
          else if (auto pos = line.find(':'); pos != line.npos) {
            auto name = lower(trim(line.substr(0, pos)));
            auto value = trim(line.substr(pos + 1));
            if (h.contains(name))
              h[name] += "," + value;
            else
              h.emplace(name, value);
          }
          return a * b;
        } catch (...) {
          return 0;
        }
      });
  curl_easy_setopt(c, CURLOPT_WRITEDATA, &t);
  curl_easy_setopt(
      c, CURLOPT_WRITEFUNCTION, +[](char* data, size_t a, size_t b, void* p) -> size_t {
        try {
          return static_cast<Transfer*>(p)->write(data, a * b);
        } catch (...) {
          return 0;
        }
      });
  auto result = curl_easy_perform(c);
  auto partial = root / "media/.partial";
  if (result == CURLE_OK && t.output.value >= 0 && t.written == t.length &&
      t.expires > time(nullptr) && generation == token) {
    timespec times[] = {{0, UTIME_NOW}, {t.expires, 0}};
    if (!futimens(t.output.value, times) && !close(std::exchange(t.output.value, -1)))
      fs::rename(partial, root / "media" / media.key);
  }
  fs::remove(partial);
}
}  // namespace

ArtworkCache::ArtworkCache(std::string egress)
    : egress_(std::move(egress)), worker_([this] { run(); }) {}
ArtworkCache::~ArtworkCache() {
  {
    std::lock_guard lock(mutex_);
    stopping_ = true;
    ++generation_;
  }
  wake_.notify_one();
  worker_.join();
}
void ArtworkCache::maintain(const Json& policy) {
  auto now = std::chrono::steady_clock::now();
  std::lock_guard lock(mutex_);
  if (policy == policy_ && now < next_sweep_) return;
  if (policy != policy_) {
    ++generation_;
    pending_.reset();
    policy_ = policy;
  }
  sweep_ = true;
  next_sweep_ = now + std::chrono::minutes(1);
  wake_.notify_one();
}
void ArtworkCache::warm(const std::string& owner, const Media& media) {
  std::lock_guard lock(mutex_);
  ++generation_;
  owner_ = owner;
  pending_ = media;
  wake_.notify_one();
}
void ArtworkCache::stop_warming() {
  std::lock_guard lock(mutex_);
  if (!owner_.empty()) {
    ++generation_;
    owner_.clear();
    pending_.reset();
  }
}
void ArtworkCache::retire(const std::string& owner) {
  std::lock_guard lock(mutex_);
  if (owner_ == owner) {
    ++generation_;
    owner_.clear();
    pending_.reset();
  }
}
void ArtworkCache::run() {
  for (;;) {
    std::unique_lock lock(mutex_);
    wake_.wait(lock, [this] { return stopping_ || sweep_ || pending_.has_value(); });
    if (stopping_) return;
    auto policy = policy_;
    auto job = std::exchange(pending_, std::nullopt);
    auto token = generation_.load();
    bool sweep = std::exchange(sweep_, false);
    lock.unlock();
    try {
      const char* home = getenv("HOME");
      if (!home || !*home) continue;
      fs::path root = fs::path(home) / ".cache/player-artworks";
      fs::create_directories(root.parent_path());
      if (mkdir(root.c_str(), 0700) && errno != EEXIST) continue;
      Fd dir(open(root.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
      if (dir.value < 0) continue;
      // Sweeps may skip an overlapping player. A presented occurrence gets
      // one warm request, so wait for the writer unless playback cancels it.
      int rc;
      while ((rc = flock(dir.value, LOCK_EX | LOCK_NB)) && errno == EWOULDBLOCK && job &&
             generation_ == token)
        std::this_thread::sleep_for(std::chrono::milliseconds(50));
      if (rc) continue;
      if (mkdirat(dir.value, "media", 0700) && errno != EEXIST) continue;
      if (!fs::is_directory(fs::symlink_status(root / "media"))) continue;
      fs::remove(root / "media/.partial");
      if (sweep) prune(root, policy, 0);
      if (job && generation_ == token) fill(root, egress_, *job, policy, generation_, token);
    } catch (const std::exception&) {
      // Concurrent external pruning or storage errors leave playback live.
    }
  }
}
}  // namespace ff
