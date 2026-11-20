#pragma once
#include "ff/json.hpp"
#include <SDL.h>
#include <memory>

namespace ff {
// Optional Wayland compositor feedback. This records compositor-reported
// presentation, not a camera measurement of the physical panel.
struct PresentedFrame {uint64_t serial=0,timestamp_ns=0;uint32_t refresh_ns=0,flags=0,clock=0;uint64_t changed_count=0;};
class PresentationFeedback {
 public:
  explicit PresentationFeedback(SDL_Window* window, bool timing_samples);
  ~PresentationFeedback();
  void before_swap(uint64_t artwork_serial);
  void poll();
  int display_fd() const;
  Json metrics() const;
  PresentedFrame latest() const;
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}
