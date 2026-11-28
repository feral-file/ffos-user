#include "ff/server.hpp"
#include <cerrno>
#include <chrono>
#include <cstring>
#include <filesystem>
#include <fcntl.h>
#include <sys/file.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

namespace ff {
namespace {
void nonblocking(int fd) {
  require(fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK) != -1 && fcntl(fd, F_SETFD, FD_CLOEXEC) != -1, "socket_error", "Cannot set socket flags");
}
bool trusted(int fd) {
#ifdef __linux__
  struct ucred credentials {}; socklen_t size = sizeof credentials;
  return getsockopt(fd, SOL_SOCKET, SO_PEERCRED, &credentials, &size) == 0 && credentials.uid == geteuid();
#else
  uid_t uid; gid_t gid; return getpeereid(fd, &uid, &gid) == 0 && uid == geteuid();
#endif
}
}
Server::Server(const std::string& path, Protocol& protocol, std::function<int64_t()> clock)
    : path_(path), protocol_(protocol), clock_(std::move(clock)) {
  if (!clock_) clock_ = [] { return std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::steady_clock::now().time_since_epoch()).count(); };
  sockaddr_un address {}; address.sun_family = AF_UNIX;
  require(path.size() < sizeof address.sun_path, "socket_error", "Socket path too long");
  auto directory = std::filesystem::path(path).parent_path();
  std::filesystem::create_directories(directory);
  struct stat st {};
  require(lstat(directory.c_str(), &st) == 0 && S_ISDIR(st.st_mode) && st.st_uid == geteuid() && (st.st_mode & 0077) == 0,
          "socket_permissions", "Socket parent must be an owned directory with mode 0700");
  try {
    lock_ = open((path + ".lock").c_str(), O_CREAT | O_RDWR | O_CLOEXEC | O_NOFOLLOW, 0600);
    require(lock_ >= 0 && flock(lock_, LOCK_EX | LOCK_NB) == 0, "already_running", "Runtime already owns the socket");
    if (lstat(path.c_str(), &st) == 0) {
      require(S_ISSOCK(st.st_mode) && st.st_uid == geteuid(), "socket_permissions", "Refusing to unlink a foreign path");
      require(unlink(path.c_str()) == 0, "socket_error", "Cannot remove stale socket");
    } else require(errno == ENOENT, "socket_error", "Cannot inspect socket path");
    listener_ = socket(AF_UNIX, SOCK_STREAM, 0);
    require(listener_ >= 0, "socket_error", "Cannot create socket"); nonblocking(listener_);
    std::memcpy(address.sun_path, path.c_str(), path.size() + 1);
    require(bind(listener_, reinterpret_cast<sockaddr*>(&address), sizeof address) == 0, "socket_error", "Cannot bind socket");
    require(chmod(path.c_str(), 0600) == 0 && listen(listener_, 8) == 0, "socket_error", "Cannot listen on private socket");
  } catch (...) { if (listener_ >= 0) close(listener_); if (lock_ >= 0) close(lock_); throw; }
}
Server::~Server() {
  for (const auto& client : clients_) close(client.fd);
  if (listener_ >= 0) { close(listener_); unlink(path_.c_str()); }
  if (lock_ >= 0) close(lock_);
}
bool Server::service(Client& c, Phase phase) {
  const auto now = clock_();
  if (c.waits_for_frame && phase == Phase::frame) {
    c.waits_for_frame = false;
    c.reading = now;
  }
  // Absolute monotonic deadlines: dribbling bytes cannot retain a slot forever.
  if ((!c.protocol.negotiated && now-c.connected >= 5000) ||
      (!c.input.empty() && !c.waits_for_frame && now-c.reading >= 5000) ||
      (!c.output.empty() && now-c.writing >= 5000)) return false;
  char buffer[16384];
  auto n = recv(c.fd, buffer, sizeof buffer, 0);
  if (n == 0) return false;
  if (n < 0 && errno != EAGAIN && errno != EWOULDBLOCK && errno != EINTR) return false;
  if (n > 0) { if(c.input.empty())c.reading=now; c.input.append(buffer, static_cast<size_t>(n)); }
  if (c.input.size() > max_frame + 4) return false;
  // Limit work per loop to preserve frame/input responsiveness under a flood.
  for (int handled = 0; handled < 8 && !c.waits_for_frame && c.input.size() >= 4; ++handled) {
    uint32_t length = 0;
    for (int i = 0; i < 4; ++i) length = (length << 8) | static_cast<unsigned char>(c.input[i]);
    if (length == 0 || length > max_frame) return false;
    if (c.input.size() < length + 4) break;
    if(c.output.empty())c.writing=now;
    try {
      auto request = parse(std::string_view(c.input).substr(4, length));
      const auto method = request.find("method");
      // These commands require renderer input or enforcement by the worker
      // lifecycle. Keep the complete frame in order on this connection; its
      // protocol deadline is still checked when the frame loop resumes.
      if (phase == Phase::swap && method != request.end() &&
          (*method == "artworkPermissions.set" || *method == "policy.set" || *method == "playback.update" ||
           *method == "input.dispatch" || *method == "media.seek")) {
        c.waits_for_frame = true;
        break;
      }
      c.output += frame(protocol_.receive(c.protocol, request));
    }
    catch (const Error& e) { c.output += frame({{"v", 1}, {"id", nullptr}, {"error", {{"code", e.code}, {"message", e.what()}}}}); }
    c.input.erase(0, length + 4);
    c.reading=now;
    if (c.output.size() > 2 * max_frame) return false;
  }
  if (c.output.empty()) if (auto event = protocol_.event(c.protocol)) {c.output = frame(*event);c.writing=now;}
  if (!c.output.empty()) {
#ifdef MSG_NOSIGNAL
    constexpr int flags = MSG_NOSIGNAL;
#else
    constexpr int flags = 0;
#endif
    auto sent = send(c.fd, c.output.data() + c.sent, c.output.size() - c.sent, flags);
    if (sent < 0 && errno != EAGAIN && errno != EWOULDBLOCK && errno != EINTR) return false;
    if (sent > 0) c.sent += static_cast<size_t>(sent);
    if (c.sent == c.output.size()) { c.output.clear(); c.sent = 0; }
  }
  return true;
}
void Server::poll(Phase phase) {
  int fd = accept(listener_, nullptr, nullptr);
  if (fd >= 0) {
    if (clients_.size() >= 8 || !trusted(fd)) close(fd);
    else { nonblocking(fd); clients_.push_back({fd, {}, {}, {}, 0, clock_(), 0, 0}); }
  }
  for (auto it = clients_.begin(); it != clients_.end();) {
    if (service(*it, phase)) ++it;
    else { protocol_.disconnected(it->protocol); close(it->fd); it = clients_.erase(it); }
  }
}

SwapControl::SwapControl(Server& server, Playback& playback, std::chrono::milliseconds progress_limit)
    : server_(server), playback_(playback), progress_limit_(progress_limit), worker_([this] { serve(); }) {}

SwapControl::~SwapControl() {
  {
    std::lock_guard lock(mutex_);
    stopping_ = true;
  }
  changed_.notify_one();
  worker_.join();
}

int64_t SwapControl::poll(Server::Phase phase) {
  const auto now = std::chrono::steady_clock::now().time_since_epoch();
  const auto ms = std::chrono::duration_cast<std::chrono::milliseconds>(now).count();
  playback_.tick(ms, wall_ms());
  server_.poll(phase);
  return ms;
}

bool SwapControl::submit(const std::function<void()>& swap) {
  const auto revision = playback_.revision();
  // Selection, timers and disconnect can synthesize a release. Keep actual
  // seat/CDP delivery on the graphics owner before its next input dispatch.
  playback_.defer_input_release(true);
  {
    std::lock_guard lock(mutex_);
    started_ = std::chrono::steady_clock::now();
  }
  changed_.notify_one();
  std::exception_ptr graphics_error;
  try {
    swap();
  } catch (...) {
    graphics_error = std::current_exception();
  }
  {
    std::lock_guard lock(mutex_);
    started_.reset();
    if (!graphics_error) graphics_error = failure_;
  }
  changed_.notify_one();
  playback_.defer_input_release(false);
  if (graphics_error) std::rethrow_exception(graphics_error);
  return playback_.revision() == revision;
}

void SwapControl::serve() {
  using namespace std::chrono_literals;
  std::unique_lock lock(mutex_);
  try {
    while (!stopping_) {
      changed_.wait(lock, [&] { return stopping_ || started_; });
      if (stopping_) break;
      const auto start = *started_;
      const auto finished = [&] { return stopping_ || started_ != start; };
      // Ordinary swaps complete before this first poll. A permanently stuck
      // swap must eventually stop satisfying the watchdog's liveness check.
      if (changed_.wait_until(lock, start + 25ms, finished)) continue;
      while (!finished() && std::chrono::steady_clock::now() < start + progress_limit_) {
        poll(Server::Phase::swap);
        changed_.wait_for(lock, 5ms, finished);
      }
      changed_.wait(lock, finished);
    }
  } catch (...) {
    failure_ = std::current_exception();
  }
}
}
