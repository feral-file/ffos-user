#pragma once
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <mutex>
#include <optional>
#include <string>
#include <thread>

#include "ff/json.hpp"

namespace ff {
// The host owns these jobs. A renderer receives only its media fd or its
// per-source browser directory, and never sees the shared player cache.
struct ArtworkRoute {
  std::string url, user_agent;
};
ArtworkRoute artwork_route(const Json& sources, const Json& item, const std::string& url);

std::string gateway_document_url(const std::string& source, const std::string& routed);

class ArtworkCache {
 public:
  struct Media {
    std::string url, user_agent, key;
    bool lan;
  };
  explicit ArtworkCache(std::string egress);
  ~ArtworkCache();
  ArtworkCache(const ArtworkCache&) = delete;
  ArtworkCache& operator=(const ArtworkCache&) = delete;
  void maintain(const Json& policy);
  void warm(const std::string& owner, const Media& media);
  void stop_warming();
  void retire(const std::string& owner);
  static std::optional<Media> media_request(const std::string& url, const std::string& user_agent,
                                            bool lan);
  static std::string browser_key(const std::string& url, bool lan);
  static void discard_media(const std::string& key);
  static int open_media(const std::string& key, int64_t max_age_days);

 private:
  void run();
  const std::string egress_;
  std::string owner_;
  Json policy_;
  std::optional<Media> pending_;
  std::mutex mutex_;
  std::condition_variable wake_;
  std::atomic<uint64_t> generation_{0};
  bool sweep_ = false, stopping_ = false;
  std::chrono::steady_clock::time_point next_sweep_{};
  std::thread worker_;
};
}  // namespace ff
