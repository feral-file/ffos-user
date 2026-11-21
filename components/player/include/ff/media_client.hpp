#pragma once
#include <sys/types.h>

#include <chrono>

#include "ff/cache_client.hpp"
#include "ff/artwork_cache.hpp"
#include "ff/media_kind.hpp"
#include "ff/presentation.hpp"

namespace ff {
// Reap only retired media children, even when no MediaClient remains. Never
// consume CEF's child exit statuses with a process-wide waitpid(-1).
void reap_media_workers();
class MediaClient {
 public:
  MediaClient(Playback& playback, Presentation& presentation, Occurrence occurrence,
              const std::string& worker, const std::string& bundle_root = "",
              const std::string& cache_mode = "", ArtworkCache* artwork_cache = nullptr,
              std::shared_ptr<const CaptureSnapshot> capture = {});
  ~MediaClient();
  void poll(bool hold = false, const Json* stage = nullptr,bool allow_direct = false);
  void seek(double seconds);
  void did_present();
  std::optional<Json> pointer_motion;
  bool pointer_clicked = false;
  FramePair frames;
  bool ready = false;
  Json status = Json::object();
  const std::string id;

 private:
  ArtworkCache* artwork_cache_;
  std::optional<ArtworkCache::Media> warm_request_;
  std::string local_media_key_;
  void stop_worker();
  Playback& playback_;
  Presentation& presentation_;
  Occurrence occurrence_;
  int socket_ = -1;
  pid_t pid_ = -1;
  bool failed_ = false, loaded_ = false;
  std::string controls_;
  uint64_t received_sequence_ = 0, presented_sequence_ = 0;
  uint64_t render_revision_ = 0;
  std::chrono::steady_clock::time_point received_ = std::chrono::steady_clock::now();
  void fail(const std::string& message);
  void controls(bool hold, const Json* stage,bool allow_direct);
  void start_live();
  void start(int cached_fd = -1);
  std::string worker_, source_override_, user_agent_;
  bool mixed_cache_ = false;
  std::string bundle_root_;
  bool fallback_live();
  std::shared_ptr<CacheInbox> cache_;
};
}  // namespace ff
