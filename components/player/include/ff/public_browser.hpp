#pragma once
#include <sys/types.h>

#include <deque>
#include <functional>

#include "ff/browser_surface.h"
#include "ff/cache_client.hpp"
#include "ff/input.hpp"
#include "ff/presentation.hpp"

namespace ff {
// One public browser process/profile/private compositor per occurrence. The
// inherited engine pipe is implementation plumbing; it is never a player API.
class PublicBrowser {
 public:
  PublicBrowser(Playback&, Presentation&, Occurrence, const std::string& launcher,
                const std::string& profile, const std::string& cache_socket,
                const std::string& resources, const std::string& backend, bool decoration = false, bool debugger = false,
                const std::string& cache_mode = "", bool render_ahead = false);
  ~PublicBrowser();
  void poll();
  void input(const Json&);
  void update();
  void did_present();
  bool has_ready_frame() const;
  bool needs_live_restart() const {
    return restart_on_online_ && playback_.online() && cache_misses_ > 0;
  }
  std::string resolved_native_mime;
  std::string cache_mode() const { return offline_ ? "isolated" : mixed_cache_ ? "mixed" : ""; }
  bool failed() const { return failed_; }
  FramePair frames;
  const std::string id;

 private:
  const std::string backend_;
  const size_t max_submissions_;
  Playback& playback_;
  Presentation& presentation_;
  Occurrence occurrence_;
  ff_browser_server* server_ = nullptr;
  ff_browser_view* view_ = nullptr;
  pid_t pid_ = -1;
  int writer_ = -1, reader_ = -1;
  std::string buffer_, session_, profile_, main_frame_, navigation_url_, document_url_, navigation_error_;
  bool initial_response_seen_ = false, decoration_ = false;
  struct Pending {
    std::string data;
    size_t offset = 0;
  };
  std::deque<Pending> writes_;
  uint64_t sequence_ = 0, load_frame_ = 0;
  size_t queued_bytes_ = 0;
  struct Reply {
    std::string method, session;
    std::function<void(const Json&)> callback;
  };
  std::map<uint64_t, Reply> replies_;
  struct Submission {
    uint64_t host_serial;
    uint32_t nested_commit;
    std::chrono::steady_clock::time_point submitted;
  };
  std::deque<Submission> submissions_;
  bool mixed_cache_ = false, restart_on_online_ = false;
  std::map<std::string, Json> mixed_requests_;
  bool loaded_ = false, failed_ = false, offline_ = false, cache_coverage_ = false,
       bundled_ = false;
  std::string bundle_prefix_, resources_ = "/usr/share/ff-player-native/renderers";
  uint64_t configured_revision_ = 0, configuration_frame_ = 0;
  bool bundled_request(const Json&, const std::string&);
  std::string cache_socket_;
  std::shared_ptr<CacheInbox> cache_ = std::make_shared<CacheInbox>();
  uint64_t compositor_gpu_passes_ = 0;
  bool health_pending_ = false;
  std::chrono::steady_clock::time_point health_at_ = std::chrono::steady_clock::now();
  size_t cache_pending_ = 0;
  uint64_t cache_hits_ = 0, cache_misses_ = 0, unsupported_worker_targets_ = 0;
  std::chrono::steady_clock::time_point started_ = std::chrono::steady_clock::now();
  void send(const std::string&, Json = Json::object(), const std::string& session = "",
            std::function<void(const Json&)> reply = {});
  void message(const Json&);
  void fail(const std::string&);
};
void reap_public_browsers();
}  // namespace ff
