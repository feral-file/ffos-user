#include "ff/cache_client.hpp"

#include <curl/curl.h>
#include <sys/socket.h>
#include <unistd.h>

#include <condition_variable>
#include <thread>
#include <vector>
namespace ff {
std::string base64(const std::string& bytes) {
  static const char alphabet[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  std::string result;
  result.reserve((bytes.size() + 2) / 3 * 4);
  for (size_t i = 0; i < bytes.size(); i += 3) {
    uint32_t v = uint32_t(uint8_t(bytes[i])) << 16;
    if (i + 1 < bytes.size()) v |= uint32_t(uint8_t(bytes[i + 1])) << 8;
    if (i + 2 < bytes.size()) v |= uint8_t(bytes[i + 2]);
    result += alphabet[v >> 18];
    result += alphabet[(v >> 12) & 63];
    result += i + 1 < bytes.size() ? alphabet[(v >> 6) & 63] : '=';
    result += i + 2 < bytes.size() ? alphabet[v & 63] : '=';
  }
  return result;
}
namespace {
constexpr size_t body_limit = 32 * 1024 * 1024;
struct Job {
  std::shared_ptr<CacheInbox> inbox;
  std::string socket, session, request, payload;
  bool open_file = false, policy = false;
};
struct Pool {
  std::mutex mutex;
  std::condition_variable ready;
  std::deque<Job> queue;
  std::vector<std::jthread> workers;
  bool stopping = false;
  explicit Pool(int count) {
    curl_global_init(CURL_GLOBAL_DEFAULT);
    for (int n = 0; n < count; ++n) workers.emplace_back([this] { run(); });
  }
  ~Pool() {
    {
      std::lock_guard lock(mutex);
      stopping = true;
    }
    ready.notify_all();
  }
  static size_t receive(char* data, size_t a, size_t b, void* context) {
    auto& result = *static_cast<std::string*>(context);
    size_t bytes = a * b;
    if (bytes > body_limit + 256 * 1024 - result.size()) return 0;
    result.append(data, bytes);
    return bytes;
  }
  static curl_socket_t open_socket(void* context, curlsocktype, struct curl_sockaddr* address) {
    if (address->family != AF_UNIX) return CURL_SOCKET_BAD;
    auto fd = socket(AF_UNIX, address->socktype | SOCK_CLOEXEC, address->protocol);
    *static_cast<curl_socket_t*>(context) = fd;
    return fd;
  }
  static int peer(void* context, char*, char*, int, int) {
    auto fd = *static_cast<curl_socket_t*>(context);
    struct ucred credentials{};
    socklen_t length = sizeof(credentials);
    return fd != CURL_SOCKET_BAD &&
                   getsockopt(fd, SOL_SOCKET, SO_PEERCRED, &credentials, &length) == 0 &&
                   credentials.uid == geteuid()
               ? CURL_PREREQFUNC_OK
               : CURL_PREREQFUNC_ABORT;
  }
  static int progress(void* context, curl_off_t, curl_off_t, curl_off_t, curl_off_t) {
    return static_cast<CacheInbox*>(context)->active ? 0 : 1;
  }
  void run() {
    while (true) {
      Job job;
      {
        std::unique_lock lock(mutex);
        ready.wait(lock, [&] { return stopping || !queue.empty(); });
        if (stopping) return;
        job = std::move(queue.front());
        queue.pop_front();
      }
      if (!job.inbox->active) continue;
      CacheReply reply;
      reply.session = job.session;
      reply.policy = job.policy;
      reply.request = job.request;
      std::string data;
      auto handle = curl_easy_init();
      if (!handle) {
        reply.error = "cache allocation failed";
      } else {
        curl_easy_setopt(handle, CURLOPT_UNIX_SOCKET_PATH, job.socket.c_str());
        curl_easy_setopt(handle, CURLOPT_URL,
                         job.policy      ? "http://localhost/v1/policy"
                         : job.open_file ? "http://localhost/v1/open"
                                         : "http://localhost/v1/replay");
        curl_easy_setopt(handle, CURLOPT_PROXY, "");
        curl_socket_t connected_socket = CURL_SOCKET_BAD;
        curl_easy_setopt(handle, CURLOPT_OPENSOCKETFUNCTION, open_socket);
        curl_easy_setopt(handle, CURLOPT_OPENSOCKETDATA, &connected_socket);
        curl_easy_setopt(handle, CURLOPT_PREREQFUNCTION, peer);
        curl_easy_setopt(handle, CURLOPT_PREREQDATA, &connected_socket);
        curl_easy_setopt(handle, CURLOPT_POSTFIELDS, job.payload.data());
        curl_easy_setopt(handle, CURLOPT_POSTFIELDSIZE_LARGE, curl_off_t(job.payload.size()));
        curl_easy_setopt(handle, CURLOPT_WRITEFUNCTION, receive);
        curl_easy_setopt(handle, CURLOPT_WRITEDATA, &data);
        curl_easy_setopt(handle, CURLOPT_NOSIGNAL, 1L);
        curl_easy_setopt(handle, CURLOPT_TIMEOUT_MS, job.open_file ? 30000L : 5000L);
        curl_easy_setopt(handle, CURLOPT_CONNECTTIMEOUT_MS, 500L);
        curl_easy_setopt(handle, CURLOPT_XFERINFOFUNCTION, progress);
        curl_easy_setopt(handle, CURLOPT_XFERINFODATA, job.inbox.get());
        curl_easy_setopt(handle, CURLOPT_NOPROGRESS, 0L);
        auto result = curl_easy_perform(handle);
        long status = 0;
        curl_easy_getinfo(handle, CURLINFO_RESPONSE_CODE, &status);
        curl_easy_cleanup(handle);
        if (result != CURLE_OK || status != 200)
          reply.error = "cache unavailable (HTTP " + std::to_string(status) + ", " +
                        curl_easy_strerror(result) + ")";
        else
          try {
            require(data.size() >= 4, "cache_protocol", "Truncated resource response");
            uint32_t length = 0;
            for (int i = 0; i < 4; ++i) length = (length << 8) | uint8_t(data[i]);
            require(length <= 128 * 1024 && data.size() >= 4 + length, "cache_protocol",
                    "Invalid resource metadata length");
            reply.metadata = Json::parse(data.substr(4, length));
            require(reply.metadata.at("version") == 1, "cache_protocol", "Cache version mismatch");
            reply.body = data.substr(4 + length);
            require(reply.body.size() <= body_limit &&
                        (job.open_file ? (reply.body.empty() && reply.metadata.contains("path"))
                                       : reply.body.size() == reply.metadata.at("size")),
                    "cache_limit", "Resource body length mismatch or over 32 MiB");
          } catch (const std::exception& e) {
            reply.error = e.what();
          }
      }
      if (job.inbox->active) {
        std::lock_guard lock(job.inbox->mutex);
        size_t total = reply.body.size();
        for (const auto& pending : job.inbox->replies) total += pending.body.size();
        if (total > 64 * 1024 * 1024) {
          reply.body.clear();
          reply.error = "cache response queue exceeds 64 MiB";
        }
        job.inbox->replies.push_back(std::move(reply));
      }
    }
  }
};
Pool& pool() {
  static Pool instance(4);
  return instance;
}
}  // namespace
bool cache_request(const std::shared_ptr<CacheInbox>& inbox, const std::string& socket,
                   const std::string& session, const std::string& request, const Json& params,
                   bool open_file, bool policy) {
  static Pool policies(2);
  auto& p = policy ? policies : pool();
  auto payload = params.dump();
  if (payload.size() > 65536) return false;
  std::lock_guard lock(p.mutex);
  if (p.queue.size() >= 128) return false;
  std::lock_guard inbox_lock(inbox->mutex);
  if (!inbox->active || inbox->outstanding >= 64) return false;
  ++inbox->outstanding;
  p.queue.push_back({inbox, socket, session, request, std::move(payload), open_file, policy});
  p.ready.notify_one();
  return true;
}
}  // namespace ff
