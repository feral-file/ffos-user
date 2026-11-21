#include "ff/artwork_cache.hpp"
#include <algorithm>
#include <cerrno>
#include <cstdlib>
#include <ctime>
#include <fcntl.h>
#include <glib.h>
#include <signal.h>
#include <spawn.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

extern char** environ;
namespace ff {
namespace {
std::string hash(const std::string& identity) {
  auto digest = g_compute_checksum_for_string(G_CHECKSUM_SHA256, identity.data(), identity.size());
  std::string key = digest ? digest : "";
  g_free(digest);
  return key;
}
std::string media_path(const std::string& key) {
  const char* home = getenv("HOME");
  if (!home || key.size() != 64 || key.find_first_not_of("0123456789abcdef") != std::string::npos) return {};
  return std::string(home) + "/.cache/player-artworks/media/" + key;
}
bool http_url(const std::string& url) { return url.starts_with("https://") || url.starts_with("http://"); }
}
std::string ArtworkCache::browser_key(const std::string& url, bool lan) {
  return http_url(url) ? hash(std::string(lan ? "lan:" : "public:") + url) : "";
}
Json ArtworkCache::media_request(const std::string& url, const std::string& user_agent, bool lan) {
  if (!http_url(url)) return nullptr;
  auto key = hash(Json::array({url, user_agent, lan}).dump());
  if (key.empty()) return nullptr;
  return {{"key", key}, {"url", url}, {"userAgent", user_agent}, {"lan", lan}};
}
ArtworkCache::ArtworkCache(std::string helper) : helper_(std::move(helper)) {}
ArtworkCache::~ArtworkCache() {
  stop(fill_);
  stop(sweep_);
  for (auto pid : retired_) while (waitpid(pid, nullptr, 0) < 0 && errno == EINTR) {}
}

void ArtworkCache::reap(Job& job) {
  if (job.pid > 0 && waitpid(job.pid, nullptr, WNOHANG) == job.pid) {
    close(job.socket);
    job = {};
  }
}
void ArtworkCache::stop(Job& job) {
  if (job.pid <= 0) return;
  close(job.socket);
  kill(job.pid, SIGKILL);
  retired_.push_back(job.pid);
  job = {};
}
void ArtworkCache::launch(Job& job, const Json& request) {
  int pair[2];
  if (socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, pair) != 0) return;
  posix_spawn_file_actions_t actions;
  posix_spawn_file_actions_init(&actions);
  posix_spawn_file_actions_adddup2(&actions, pair[1], STDIN_FILENO);
  char* argv[] = {helper_.data(), nullptr};
  pid_t pid;
  auto rc = posix_spawn(&pid, helper_.c_str(), &actions, nullptr, argv, environ);
  posix_spawn_file_actions_destroy(&actions);
  close(pair[1]);
  if (rc != 0) { close(pair[0]); return; }
  job = {pair[0], pid};
  auto packet = request.dump();
  if (send(pair[0], packet.data(), packet.size(), MSG_NOSIGNAL | MSG_DONTWAIT) < 0) stop(job);
}
void ArtworkCache::maintain(const Json& policy) {
  std::erase_if(retired_, [](pid_t pid) { return waitpid(pid, nullptr, WNOHANG) == pid; });
  auto now = std::chrono::steady_clock::now();
  if (policy == policy_ && now < next_sweep_) return;
  if (policy != policy_) { stop(fill_); policy_ = policy; }
  reap(fill_);
  reap(sweep_);
  if (sweep_.pid < 0) launch(sweep_, policy_);
  next_sweep_ = now + std::chrono::minutes(1);
}
void ArtworkCache::warm(const std::string& owner, const Json& request) {
  stop(fill_);
  owner_ = owner;
  Json job = policy_;
  job.update(request);
  launch(fill_, job);
}
void ArtworkCache::stop_warming() {
  stop(fill_);
  owner_.clear();
}
void ArtworkCache::retire(const std::string& owner) {
  if (owner == owner_) stop_warming();
}
void ArtworkCache::discard_media(const std::string& key) {
  unlink(media_path(key).c_str());
}
int ArtworkCache::open_media(const std::string& key, int64_t max_age_days) {
  int fd = open(media_path(key).c_str(),
                O_RDONLY | O_NONBLOCK | O_CLOEXEC | O_NOFOLLOW);
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
}
