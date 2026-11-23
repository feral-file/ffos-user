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
                const std::string& profile, const std::string& bundle_root,
                const std::string& resources, const std::string& backend, bool decoration = false, bool debugger = false,
                const std::string& cache_mode = "", bool render_ahead = false, bool allow_direct = true, Json streams = Json::object(),
                std::shared_ptr<const CaptureSnapshot> capture = {});
  ~PublicBrowser();
  void poll();
  void input(const Json&);
  void update();
  void did_present();
  bool has_ready_frame() const;
  bool direct() const { return feedback_fd_>=0; }
  bool needs_live_restart() const {
    return restart_on_online_ && playback_.online() && cache_misses_ > 0;
  }
  struct PointerSample { int x, y; bool clicked; };
  std::optional<PointerSample> physical_pointer;
  std::string resolved_native_mime;
  std::shared_ptr<const CaptureSnapshot> capture() const { return capture_; }
  std::string cache_mode() const { return offline_ ? "isolated" : mixed_cache_ ? "mixed" : ""; }
  bool needs_compositor() const {
    if(!direct() && resize_started_at_ && !has_ready_frame() && playback_.overlay().is_null() &&
       !playback_.service_failure() && !playback_.thermal_hold() &&
       std::chrono::steady_clock::now()-*resize_started_at_>std::chrono::seconds(2))
      return true;
    return direct() && (failed_ || !playback_.overlay().is_null() || playback_.service_failure() ||
           playback_.thermal_hold() || occurrence_.display!=initial_display_ ||
           (!has_ready_frame() && loaded_ && std::chrono::steady_clock::now()-loaded_at_>std::chrono::seconds(3)));
  }
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
  int feedback_fd_=-1, admission_fd_=-1;
  bool window_admitted_=false;
  uint64_t direct_frames_=0, last_presentation_ns_=0, navigation_load_ns_=0;
  Json initial_display_;
  Json streams_;
  std::shared_ptr<const CaptureSnapshot> capture_;
  std::chrono::steady_clock::time_point loaded_at_;
  std::optional<std::chrono::steady_clock::time_point> resize_started_at_;
  double mouse_x_=0,mouse_y_=0;
  int writer_ = -1, reader_ = -1;
  std::string buffer_, session_, profile_, main_frame_, main_document_request_, navigation_url_, document_url_, navigation_error_;
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
  bool capture_replay_ = false, mixed_cache_ = false, restart_on_online_ = false;
  std::map<std::string, Json> mixed_requests_;
  bool loaded_ = false, failed_ = false, offline_ = false, cache_coverage_ = false,
       bundled_ = false;
  std::string bundle_prefix_, resources_ = "/usr/share/ff-player-native/renderers";
  uint64_t configured_revision_ = 0, configuration_frame_ = 0;
  bool bundled_request(const Json&, const std::string&);
  std::string bundle_root_;
  std::shared_ptr<CacheInbox> cache_ = std::make_shared<CacheInbox>();
  uint64_t compositor_gpu_passes_ = 0;
  bool health_pending_ = false;
  std::chrono::steady_clock::time_point health_at_ = std::chrono::steady_clock::now();
  struct PausedRequest {
    std::string session, request;
    Json query;
  };
  std::deque<PausedRequest> cache_queue_;
  bool cache_reading_ = false;
  uint64_t cache_hits_ = 0, cache_misses_ = 0, unsupported_worker_targets_ = 0;
  std::chrono::steady_clock::time_point started_ = std::chrono::steady_clock::now();
  void send(const std::string&, Json = Json::object(), const std::string& session = "",
            std::function<void(const Json&)> reply = {});
  bool continue_mixed_request(const std::string& session, const std::string& request);
  void continue_live(const std::string& request, const std::string& session, const Json& network);
  void message(const Json&);
  void fail(const std::string&);
};
void reap_public_browsers();
}  // namespace ff
