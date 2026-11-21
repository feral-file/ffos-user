#include "ff/cache_client.hpp"
#include "ff/media_kind.hpp"

#include <glib.h>
#include <filesystem>
#include <fstream>
#include <ctime>

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
  std::string root, session, request, payload;
  bool open_file = false, policy = false;
};
struct Pool {
  std::mutex mutex;
  std::condition_variable ready;
  std::deque<Job> queue;
  std::vector<std::jthread> workers;
  bool stopping = false;
  explicit Pool(int count) {
    for (int n = 0; n < count; ++n) workers.emplace_back([this] { run(); });
  }
  ~Pool() {
    {
      std::lock_guard lock(mutex);
      stopping = true;
    }
    ready.notify_all();
    for (auto& worker : workers) worker.join();
  }
  // The offline capture store publishes immutable content-addressed blobs and
  // atomically replaces its item manifests. Read them as local artwork inputs.
  static void load(const Job& job, CacheReply& reply) {
    auto query = Json::parse(job.payload);
    auto source = query.at("source").get<std::string>();
    auto url = query.at("url").get<std::string>();
    auto root = std::filesystem::path(job.root);
    char* key = g_compute_checksum_for_string(G_CHECKSUM_SHA256, source.data(), source.size());
    auto path = root / "items" / (std::string(key) + ".json");
    g_free(key);
    reply.metadata = {{"requestUrl", url}};
    if (job.policy && url != source) return;
    std::ifstream stream(path, std::ios::binary);
    if (!stream && errno == ENOENT && job.policy) return;
    require(stream.good() && std::filesystem::file_size(path) <= 16 * 1024 * 1024, "bundle", "Cannot read capture manifest");
    auto record = Json::parse(stream);
    require(record.at("item").at("source") == source, "bundle", "Capture identity mismatch");
    if (job.policy) {
      const Json* entry = nullptr;
      bool browser_ready = true;
      for (const auto& resource : record.at("resources")) {
        if (resource.value("method", "GET") == "GET" &&
            (resource.value("url", "") == source || resource.value("url", "") == record.value("entry", source)))
          entry = &resource;
        if (resource.value("browserPolicyVersion", 0) != 2) browser_ready = false;
        if (record.value("automatic", false)) {
          auto expiry = g_date_time_new_from_iso8601(resource.value("freshUntil", "").c_str(), nullptr);
          bool fresh = expiry && g_date_time_to_unix(expiry) > std::time(nullptr);
          if (expiry) g_date_time_unref(expiry);
          if (!fresh) return;
        }
      }
      if (!entry) return;
      auto type = entry->value("contentType", "");
      if (type.empty()) type = entry->value("sniffedContentType", "");
      if (!browser_ready && !native_media(Json{{"source", source}, {"mimeType", type}})) return;
      reply.metadata["replayMode"] = record.value("automatic", false) ? "mixed" : "isolated";
      return;
    }
    auto method = query.value("method", "");
    require(method == "GET" || method == "HEAD", "bundle", "Unsupported replay method");
    for (const auto& [name, value] : query.at("headers").items()) {
      if (value != "" && (!g_ascii_strcasecmp(name.c_str(), "Authorization") ||
                          !g_ascii_strcasecmp(name.c_str(), "Cookie") || !g_ascii_strcasecmp(name.c_str(), "Range")))
        throw Error("bundle", "Request variant requires live origin");
    }
    const Json* found = nullptr;
    for (const auto& resource : record.at("resources")) {
      if (resource.at("url") == url && resource.value("method", "GET") == "GET") {
        require(!found, "bundle", "Ambiguous capture entry");
        found = &resource;
      }
    }
    require(found, "bundle", "Resource absent from capture");
    if (record.value("automatic", false)) {
      auto until = found->value("freshUntil", "");
      auto expiry = g_date_time_new_from_iso8601(until.c_str(), nullptr);
      bool fresh = expiry && g_date_time_to_unix(expiry) > std::time(nullptr);
      if (expiry) g_date_time_unref(expiry);
      require(fresh, "bundle", "Cached resource expired");
    }
    // Controld's older manifests omit CSP and repeated response headers.
    // They can supply native media bytes, but never synthesize a browser response.
    auto mime = found->value("contentType", found->value("sniffedContentType", ""));
    if (!job.open_file && url == source && native_media(Json{{"source", source}, {"mimeType", mime}})) {
      reply.metadata = {{"nativeMime", mime}};
      return;
    }
    require(job.open_file || found->value("browserPolicyVersion", 0) == 2, "bundle", "Browser capture requires recapture for original response headers");
    auto headers = found->value("headerFields", Json::array());
    reply.metadata = {{"status", found->value("status", 200)}, {"headers", headers},
                      {"complete", record.at("coverage").value("complete", false)}};
    if (found->contains("redirectTo") || method == "HEAD") return;
    auto hash = found->at("sha256").get<std::string>();
    require(hash.size() == 64 && hash.find_first_not_of("0123456789abcdef") == hash.npos, "bundle", "Invalid blob digest");
    auto blob = root / "blobs" / hash;
    auto size = std::filesystem::file_size(blob);
    require(job.open_file || size <= body_limit, "bundle", "Browser resource exceeds replay limit");
    std::ifstream body(blob, std::ios::binary);
    require(body.good(), "bundle", "Cannot open captured resource");
    std::unique_ptr<GChecksum, decltype(&g_checksum_free)> digest(g_checksum_new(G_CHECKSUM_SHA256), g_checksum_free);
    char block[65536];
    size_t bytes = 0;
    while (job.inbox->active && body) {
      body.read(block, sizeof(block));
      auto n = body.gcount();
      bytes += n;
      require(bytes <= size, "bundle", "Blob changed during read");
      g_checksum_update(digest.get(), reinterpret_cast<const guchar*>(block), n);
      if (!job.open_file) reply.body.append(block, n);
    }
    require(body.eof() && bytes == size && hash == g_checksum_get_string(digest.get()), "bundle", "Capture checksum mismatch");
    if (job.open_file) {
      reply.metadata["path"] = blob.string();
      reply.metadata["size"] = size;
    }
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
      try {
        load(job, reply);
      } catch (const std::exception& e) {
        reply.body.clear();
        reply.error = e.what();
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
bool cache_request(const std::shared_ptr<CacheInbox>& inbox, const std::string& root,
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
  p.queue.push_back({inbox, root, session, request, std::move(payload), open_file, policy});
  p.ready.notify_one();
  return true;
}
}  // namespace ff
