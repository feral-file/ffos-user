#include "ff/cache_client.hpp"
#include "ff/media_kind.hpp"
#include "ff/profile_gc.hpp"
#include <sys/stat.h>
#include <set>

#include <glib.h>
#include <filesystem>
#include <fstream>
#include <ctime>
#include <charconv>
#include <algorithm>

#include <condition_variable>
#include <thread>
#include <vector>
namespace ff {
CaptureSnapshot::~CaptureSnapshot() {
  if (!root.empty()) retire_browser_profile(root);
}
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
  std::shared_ptr<const CaptureSnapshot> capture;
};
// Hardlinks retain inodes without copying artwork bytes. Keep the lease on
// the blob filesystem even when the operator uses a separate cache mount.
std::string pin_blobs(const std::filesystem::path& store, const Json& record,
                      const std::shared_ptr<CacheInbox>& owner) {
  namespace fs = std::filesystem;
  auto absolute = fs::canonical(store);
  auto parent = absolute.parent_path();
  struct stat store_stat{}, parent_stat{};
  require(!stat(absolute.c_str(), &store_stat) && !stat(parent.c_str(), &parent_stat),
          "bundle", "Cannot locate capture filesystem");
  auto leases = store_stat.st_dev == parent_stat.st_dev
      ? parent / ("." + absolute.filename().string() + "-player-leases")
      : absolute / ".player-leases";
  // One kiosk owns the product socket. Retire leftovers from its previous
  // process once per store, before exposing any current occurrence paths.
  static std::mutex mutex;
  static std::set<fs::path> initialized;
  {
    std::lock_guard lock(mutex);
    if (initialized.insert(leases).second) sweep_browser_profiles(leases.string());
  }
  auto lease = leases / uuid();
  auto blobs = lease / "blobs";
  fs::create_directories(blobs);
  try {
    for (const auto& resource : record.at("resources")) {
      require(owner->active, "bundle", "Artwork selection was cancelled");
      auto hash = resource.value("sha256", "");
      if (hash.size() != 64 || hash.find_first_not_of("0123456789abcdef") != hash.npos) continue;
      std::error_code error;
      fs::create_hard_link(absolute / "blobs" / hash, blobs / hash, error);
      if (error && error != std::errc::file_exists && error != std::errc::no_such_file_or_directory)
        throw fs::filesystem_error("Cannot retain captured blob", blobs / hash, error);
    }
  } catch (...) {
    retire_browser_profile(lease.string());
    throw;
  }
  return lease.string();
}
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
    reply.metadata = {{"requestUrl", url}};
    if (job.policy && url != source) return;
    const auto capsule_id = query.value("capsuleId", "");
    auto capture = job.capture;
    std::shared_ptr<CaptureSnapshot> loaded;
    if (!capture) {
      char* key = g_compute_checksum_for_string(G_CHECKSUM_SHA256, source.data(), source.size());
      auto path = root / "items" / (std::string(key) + ".json");
      g_free(key);
      std::ifstream stream(path, std::ios::binary);
      if (!stream && errno == ENOENT && job.policy && capsule_id.empty()) return;
      require(stream.good() && std::filesystem::file_size(path) <= 16 * 1024 * 1024, "bundle", "Cannot read capture manifest");
      auto manifest = Json::parse(stream);
      const bool wrapped = manifest.value("automaticCacheVersion", 0) != 0;
      if (wrapped) require(manifest.at("automaticCacheVersion") == 1 && manifest.contains("record") &&
                           manifest.at("record").value("automatic", false), "bundle", "Unsupported automatic capture");
      loaded = std::make_shared<CaptureSnapshot>();
      loaded->record = wrapped ? std::move(manifest.at("record")) : std::move(manifest);
      capture = loaded;
    }
    const auto& record = capture->record;
    if (!capture->root.empty()) root = capture->root;
    require(record.at("item").at("source") == source, "bundle", "Capture identity mismatch");
    if (job.policy) {
      if (!capsule_id.empty()) {
        require(record.contains("capsule") && record.at("capsule").at("archiveSHA256") == capsule_id,
                "bundle", "Offline capture does not match the requested capsule");
      }
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
      if (!browser_ready && !native_media(Json{{"source", source}, {"mimeType", type}})) {
        require(record.value("automatic", false), "bundle", "Downloaded browser capture needs recapture for original response headers");
        return;
      }
      if (loaded) loaded->root = pin_blobs(root, record, job.inbox);
      root = capture->root;
      // Large browser GETs use an original-origin CEF stream. Only the
      // current artwork's captured blobs may enter its private mount namespace.
      if (browser_ready && !native_media(Json{{"source", source}, {"mimeType", type}})) {
        Json streams = Json::object();
        for (const auto& resource : record.at("resources")) {
          auto hash = resource.value("sha256", "");
          if (resource.value("method", "GET") != "GET" || hash.size() != 64 ||
              hash.find_first_not_of("0123456789abcdef") != hash.npos || resource.contains("redirectTo")) continue;
          std::error_code error;
          auto size = std::filesystem::file_size(root / "blobs" / hash, error);
          if (error || size <= body_limit) continue;
          auto target = resource.at("url").get<std::string>();
          require(!streams.contains(target), "bundle", "Ambiguous streamed resource");
          int64_t until = 0;
          if (record.value("automatic", false)) {
            auto expiry = g_date_time_new_from_iso8601(resource.at("freshUntil").get_ref<const std::string&>().c_str(), nullptr);
            if (!expiry) continue;
            until = g_date_time_to_unix(expiry);
            g_date_time_unref(expiry);
          }
          streams[target] = {{"hash", hash}, {"until", until}, {"status", resource.value("status", 200)},
                             {"mime", resource.value("contentType", "")}, {"headers", resource.value("headerFields", Json::array())}};
        }
        if (!streams.empty()) reply.metadata["streams"] = std::move(streams);
      }
      // The occurrence keeps the manifest that authorized its read-only mounts.
      // A concurrent capture publishes a new representation for the next play.
      reply.capture = std::move(capture);
      reply.metadata["replayMode"] = record.value("automatic", false) ? "mixed" : "isolated";
      return;
    }
    auto method = query.value("method", "");
    require(method == "GET" || method == "HEAD" || method == "OPTIONS", "bundle", "Unsupported replay method");
    std::string range;
    for (const auto& [name, value] : query.at("headers").items()) {
      if (!g_ascii_strcasecmp(name.c_str(), "Range")) range = value.get<std::string>();
      if (value != "" && (!g_ascii_strcasecmp(name.c_str(), "Authorization") ||
                          !g_ascii_strcasecmp(name.c_str(), "Cookie")))
        throw Error("bundle", "Request variant requires live origin");
    }
    const Json* found = nullptr;
    for (int hop = 0; hop < 16; ++hop) {
      found = nullptr;
      const Json* head_fallback = nullptr;
      for (const auto& resource : record.at("resources")) {
        if (resource.at("url") != url) continue;
        auto captured_method = resource.value("method", "GET");
        if (method == "HEAD" && captured_method == "GET") {
          require(!head_fallback, "bundle", "Ambiguous GET for HEAD fallback");
          head_fallback = &resource;
        }
        if (captured_method == method) {
          require(!found, "bundle", "Ambiguous capture entry");
          found = &resource;
        }
      }
      if (!found) found = head_fallback;
      require(found, "bundle", "Resource absent from capture");
      if (record.value("automatic", false)) {
        auto expiry = g_date_time_new_from_iso8601(found->value("freshUntil", "").c_str(), nullptr);
        bool fresh = expiry && g_date_time_to_unix(expiry) > std::time(nullptr);
        if (expiry) g_date_time_unref(expiry);
        require(fresh, "bundle", "Cached resource expired");
      }
      if (!job.open_file || !found->contains("redirectTo")) break;
      char* target = g_uri_resolve_relative(url.c_str(), found->at("redirectTo").get_ref<const std::string&>().c_str(),
                                            G_URI_FLAGS_ENCODED, nullptr);
      require(target, "bundle", "Invalid capture redirect");
      url = target;
      g_free(target);
      require(hop < 15, "bundle", "Too many capture redirects");
    }
    // Controld's older manifests omit CSP and repeated response headers.
    // They can supply native media bytes, but never synthesize a browser response.
    auto mime = found->value("contentType", found->value("sniffedContentType", ""));
    if (!job.open_file && method == "GET" && url == source && native_media(Json{{"source", source}, {"mimeType", mime}})) {
      reply.metadata = {{"nativeMime", mime}};
      return;
    }
    require(job.open_file || found->value("browserPolicyVersion", 0) == 2, "bundle", "Browser capture requires recapture for original response headers");
    auto headers = found->value("headerFields", Json::array());
    reply.metadata = {{"status", found->value("status", 200)}, {"headers", headers},
                      {"complete", record.at("coverage").value("complete", false)}};
    if (found->contains("redirectTo") || method == "HEAD" || method == "OPTIONS") return;
    auto hash = found->at("sha256").get<std::string>();
    require(hash.size() == 64 && hash.find_first_not_of("0123456789abcdef") == hash.npos, "bundle", "Invalid blob digest");
    auto blob = root / "blobs" / hash;
    reply.capture = capture;
    auto size = std::filesystem::file_size(blob);
    uintmax_t first = 0, end = size;
    auto& fields = reply.metadata["headers"];
    if (!job.open_file) {
      for (auto it=fields.begin(); it!=fields.end();) {
        auto name=it->value("name", "");
        if (!g_ascii_strcasecmp(name.c_str(),"Content-Length") || !g_ascii_strcasecmp(name.c_str(),"Content-Range")) it=fields.erase(it);
        else ++it;
      }
    }
    if (!job.open_file && !range.empty()) {
      require(range.starts_with("bytes=") && range.find(',') == range.npos, "bundle", "Unsupported range");
      auto dash = range.find('-', 6);
      require(dash != range.npos && size > 0, "bundle", "Invalid range");
      auto number = [](std::string_view text) {
        uintmax_t value = 0;
        auto [ptr, ec] = std::from_chars(text.data(), text.data() + text.size(), value);
        require(ec == std::errc{} && ptr == text.data()+text.size(), "bundle", "Invalid byte offset");
        return value;
      };
      if (dash == 6) first = size - std::min(size, number(range.substr(7)));
      else {
        first = number(range.substr(6, dash - 6));
        if (dash + 1 < range.size()) end = std::min(size - 1, number(range.substr(dash + 1))) + 1;
      }
      if (first >= end) {
        reply.metadata["status"]=416;
        fields.push_back({{"name","Content-Range"},{"value","bytes */"+std::to_string(size)}});
        return;
      }
      end = first + std::min<uintmax_t>(end-first, body_limit);
      fields.push_back({{"name","Content-Range"},{"value","bytes "+std::to_string(first)+"-"+std::to_string(end-1)+"/"+std::to_string(size)}});
      fields.push_back({{"name","Content-Length"},{"value",std::to_string(end-first)}});
      reply.metadata["status"]=206;
    } else if (reply.metadata.at("status") == 206) reply.metadata["status"]=200;
    if (!job.open_file && end-first > body_limit) {
      reply.metadata["streamHash"] = hash;
      reply.metadata["url"] = url;
      return;
    }
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
      auto begin = std::max<uintmax_t>(first, bytes - n);
      auto finish = std::min<uintmax_t>(end, bytes);
      if (!job.open_file && finish > begin) reply.body.append(block + (begin - (bytes - n)), finish - begin);
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
                   bool open_file, bool policy, std::shared_ptr<const CaptureSnapshot> capture) {
  static Pool policies(2);
  auto& p = policy ? policies : pool();
  auto payload = params.dump();
  if (payload.size() > 65536) return false;
  std::lock_guard lock(p.mutex);
  if (p.queue.size() >= 128) return false;
  std::lock_guard inbox_lock(inbox->mutex);
  if (!inbox->active || inbox->outstanding >= 64) return false;
  ++inbox->outstanding;
  p.queue.push_back({inbox, root, session, request, std::move(payload), open_file, policy, std::move(capture)});
  p.ready.notify_one();
  return true;
}
}  // namespace ff
