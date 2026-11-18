#pragma once
#include "ff/json.hpp"
#include <SDL.h>
#include <memory>

namespace ff {
// Optional Wayland compositor feedback. This records compositor-reported
// presentation, not a camera measurement of the physical panel.
class PresentationFeedback {
 public:
  explicit PresentationFeedback(SDL_Window* window, bool timing_samples);
  ~PresentationFeedback();
  void before_swap(uint64_t artwork_serial);
  void poll();
  Json metrics() const;
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}
