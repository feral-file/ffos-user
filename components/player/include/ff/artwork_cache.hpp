#pragma once
#include <chrono>
#include <string>
#include <vector>
#include <sys/types.h>
#include "ff/json.hpp"

namespace ff {
// The host owns these jobs. A renderer receives only its media fd or its
// per-source browser directory, and never sees the shared player cache.
class ArtworkCache {
 public:
  explicit ArtworkCache(std::string helper);
  ~ArtworkCache();
  ArtworkCache(const ArtworkCache&) = delete;
  ArtworkCache& operator=(const ArtworkCache&) = delete;
  void maintain(const Json& policy);
  void warm(const std::string& owner, const Json& request);
  void stop_warming();
  void retire(const std::string& owner);
  static Json media_request(const std::string& url, const std::string& user_agent, bool lan);
  static std::string browser_key(const std::string& url, bool lan);
  static void discard_media(const std::string& key);
  static int open_media(const std::string& key, int64_t max_age_days);

 private:
  struct Job { int socket = -1; pid_t pid = -1; };
  void launch(Job& job, const Json& request);
  void stop(Job& job);
  static void reap(Job& job);
  std::string helper_, owner_;
  Json policy_;
  Job fill_, sweep_;
  std::vector<pid_t> retired_;
  std::chrono::steady_clock::time_point next_sweep_{};
};
}
