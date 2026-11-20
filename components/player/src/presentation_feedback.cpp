#include "ff/presentation_feedback.hpp"
#include "ff/presentation.hpp"
#include "presentation-time-client-protocol.h"
#include <SDL_syswm.h>
#include <wayland-client.h>
#include <cstring>
#include <set>
#include <deque>
#include <time.h>

namespace ff {
struct PresentationFeedback::Impl {
  wl_display* display = nullptr;
  wl_event_queue* queue = nullptr;
  wl_surface* surface = nullptr;
  wl_registry* registry = nullptr;
  wp_presentation* presentation = nullptr;
  struct Pending { Impl* owner; struct wp_presentation_feedback* feedback; uint64_t serial; };
  std::set<Pending*> pending;
  FrameTiming intervals;
  PresentedFrame latest;
  bool timing_samples = false;
  std::deque<uint64_t> changed_timestamps;
  uint64_t truncated = 0, changed_count = 0;
  uint64_t presented = 0, discarded = 0, omitted = 0, last_ns = 0, last_serial = 0;
  uint32_t clock = 0, last_flags = 0, refresh_ns = 0;
  static void global(void* data, wl_registry* registry, uint32_t name, const char* interface, uint32_t version) {
    auto& self = *static_cast<Impl*>(data);
    if (std::strcmp(interface, wp_presentation_interface.name) != 0 || self.presentation) return;
    self.presentation = static_cast<wp_presentation*>(wl_registry_bind(registry, name, &wp_presentation_interface, std::min(version, 1u)));
    static const wp_presentation_listener listener = {
      [](void* data, wp_presentation*, uint32_t clock) { static_cast<Impl*>(data)->clock = clock; }
    };
    wp_presentation_add_listener(self.presentation, &listener, &self);
  }
  static void finish(Pending* p) {
    wp_presentation_feedback_destroy(p->feedback);
    p->owner->pending.erase(p); delete p;
  }
  static void shown(void* data, struct wp_presentation_feedback*, uint32_t hi, uint32_t lo, uint32_t ns,
                    uint32_t refresh, uint32_t, uint32_t, uint32_t flags) {
    auto* p = static_cast<Pending*>(data); auto& self = *p->owner;
    ++self.presented; self.last_flags = flags; self.refresh_ns = refresh;
    const auto timestamp = ((uint64_t(hi)<<32) | lo)*1000000000 + ns;
    if (p->serial && p->serial != self.last_serial) {
      ++self.changed_count;
      if (self.last_ns && timestamp > self.last_ns) self.intervals.add((timestamp-self.last_ns)/1e6);
      self.last_ns = timestamp; self.last_serial = p->serial;
      if (self.timing_samples) {
        if (self.changed_timestamps.size() == 8192) {self.changed_timestamps.pop_front();++self.truncated;}
        self.changed_timestamps.push_back(timestamp);
      }
    }
    if(p->serial && p->serial>=self.latest.serial)
      self.latest={p->serial,timestamp,refresh,flags,self.clock,self.changed_count};
    finish(p);
  }
  static void dropped(void* data, struct wp_presentation_feedback*) {
    auto* p = static_cast<Pending*>(data); ++p->owner->discarded; finish(p);
  }
  ~Impl() {
    while (!pending.empty()) finish(*pending.begin());
    if (presentation) wp_presentation_destroy(presentation);
    if (registry) wl_registry_destroy(registry);
    if (queue) wl_event_queue_destroy(queue);
  }
};
PresentationFeedback::PresentationFeedback(SDL_Window* window, bool timing_samples):impl_(std::make_unique<Impl>()) {
  impl_->timing_samples=timing_samples;
  SDL_SysWMinfo info{}; SDL_VERSION(&info.version);
  if (!SDL_GetWindowWMInfo(window,&info) || info.subsystem != SDL_SYSWM_WAYLAND) return;
  auto& self=*impl_; self.display=info.info.wl.display; self.surface=info.info.wl.surface;
  self.queue=wl_display_create_queue(self.display);
  if (!self.queue) return;
  self.registry=wl_display_get_registry(self.display);
  if (!self.registry) return;
  wl_proxy_set_queue(reinterpret_cast<wl_proxy*>(self.registry),self.queue);
  static const wl_registry_listener listener = {Impl::global, [](void*,wl_registry*,uint32_t){}};
  wl_registry_add_listener(self.registry,&listener,&self);
  // SDL owns socket reads. A separate queue keeps our dispatch from invoking
  // SDL callbacks recursively. Startup discovery is the only round trip.
  if (wl_display_roundtrip_queue(self.display,self.queue) < 0) self.surface=nullptr;
}
PresentationFeedback::~PresentationFeedback() = default;
void PresentationFeedback::before_swap(uint64_t serial) {
  auto& self=*impl_;
  if (!self.presentation || !self.surface) return;
  if (self.pending.size()>=64) {++self.omitted;return;}
  auto* p=new Impl::Pending{&self,wp_presentation_feedback(self.presentation,self.surface),serial};
  self.pending.insert(p);
  static const wp_presentation_feedback_listener listener = {
    [](void*,struct wp_presentation_feedback*,wl_output*){}, Impl::shown, Impl::dropped
  };
  wp_presentation_feedback_add_listener(p->feedback,&listener,p);
}
void PresentationFeedback::poll() {
  auto& self=*impl_;
  if (self.display && self.queue) wl_display_dispatch_queue_pending(self.display,self.queue);
}
PresentedFrame PresentationFeedback::latest() const { return impl_->latest; }
Json PresentationFeedback::metrics() const {
  const auto& s=*impl_;
  timespec clock_now{}, realtime_now{};
  const bool clock_valid = clock_gettime(static_cast<clockid_t>(s.clock), &clock_now)==0 &&
                           clock_gettime(CLOCK_REALTIME,&realtime_now)==0;
  return {{"clockSampleValid",clock_valid},
    {"clockSampleNs",uint64_t(clock_now.tv_sec)*1000000000+clock_now.tv_nsec},
    {"realtimeSampleNs",uint64_t(realtime_now.tv_sec)*1000000000+realtime_now.tv_nsec},
    {"changedTimestampsNs",s.changed_timestamps},{"truncatedTimestamps",s.truncated},
    {"available",s.presentation!=nullptr&&s.surface!=nullptr},{"clockId",s.clock},
    {"presented",s.presented},{"discarded",s.discarded},{"omitted",s.omitted},
    {"lastFlags",s.last_flags},{"refreshNanoseconds",s.refresh_ns},{"changedFrameIntervals",s.intervals.json()}};
}
}
