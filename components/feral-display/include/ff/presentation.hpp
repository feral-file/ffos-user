#pragma once
#include "ff/playback.hpp"
#include "ff/device_ui.hpp"
#include "ff/display.hpp"
#include "ff/surface.hpp"
#include "ff/frame_clock.hpp"
#include "ff/cage_overlay.hpp"
#include "ff/presentation_feedback.hpp"
#include <SDL.h>
#include <GLES3/gl3.h>
#include <EGL/egl.h>
#include <array>
#include <chrono>
#include <tuple>

namespace ff {
// Bounded timing histogram: one millisecond bins, final bin is overflow.
// Measures host work/submission, never physical display presentation.
struct FrameTiming {
  std::array<uint64_t, 1001> bins{};
  uint64_t count = 0;
  double total = 0, maximum = 0;
  void add(double milliseconds);
  Json json() const;
};
struct ArtworkBlend {
  const Surface* outgoing = nullptr;
  const Json* stage = nullptr;
  float outgoing_opacity = 0;
  float opacity = 1;
};
struct FrameSubmission {
  bool overlay;
  const Surface* artwork;
};
class Presentation {
 public:
  explicit Presentation(bool diagnostic_cpu, bool timing_samples, const std::string& fonts);
  ~Presentation();
  void accelerated(Surface& surface, const DmaFrame& info, std::shared_ptr<void> producer_buffer = {},
                   bool direct_import = false);
  bool import_read_complete(const Surface& surface);
  bool copy_ready(Surface& surface);
  void coalesced_frame() { ++coalesced_frames_; }
  void software(Surface& surface, const void* data, int width, int height);
  void destroy(Surface& surface);
  std::optional<FrameSubmission> draw(const Surface* artwork, const Occurrence* occurrence, const Playback& playback,
            const Surface* backdrop = nullptr, float backdrop_opacity = 0,
            const ArtworkBlend& blend = {}, bool external = false);
  void submit(const FrameSubmission& frame);
  bool events(const std::function<void(const Json&)>& input);
  Bounds bounds(const Json& display) const;
  bool can_present_direct(const Occurrence& occurrence,const Playback& playback) const;
  int width() const { return width_; }
  int height() const { return height_; }
  void pointer(double x, double y, bool button_down);
  void pointer_position(double x, double y) { pointer_x_ = x; pointer_y_ = y; }
  Json metrics() const;
  void browser_feedback(double elapsed_ms, bool fallback);
  uint64_t swap_serial() const { return swaps_; }
  uint64_t frame_serial() const { return frame_serial_; }
  PresentedFrame outcome(uint64_t serial) const { return feedback_->outcome(serial); }
  PresentedFrame latest_presented() const { return feedback_->latest(); }
 private:
  std::unique_ptr<PresentationFeedback> feedback_;
  std::unique_ptr<CageOverlay> browser_overlay_;
  uint64_t frame_serial_ = 0;
  SDL_Window* window_ = nullptr;
  SDL_GLContext context_ = nullptr;
  int width_ = 0, height_ = 0;
  bool diagnostic_cpu_;
  GLuint program_ = 0, ui_ = 0, pointer_texture_ = 0;
  std::string ui_key_;
  std::string drawing_occurrence_, card_cast_;
  TitleCardClock card_clock_;
  bool card_visible_ = false;
  int ui_width_ = 0, ui_height_ = 0;
  double pointer_x_ = 0, pointer_y_ = 0;
  bool pointer_texture_pressed_ = false;
  uint64_t pointer_press_until_ = 0;
  int pointer_size_ = 0, pointer_hotspot_ = 0;
  uint64_t pointer_active_ = 0, gpu_copies_ = 0, cpu_copies_ = 0, swaps_ = 0;
  uint64_t direct_imports_ = 0;
  uint64_t coalesced_frames_ = 0;
  double copy_ms_ = 0;
  FrameTiming copy_fence_timing_, copy_timing_, swap_timing_, frame_timing_;
  FrameTiming browser_feedback_timing_;
  uint64_t browser_feedback_fallbacks_ = 0;
  std::chrono::steady_clock::time_point last_frame_{};
  FrameClock draw_clock_;
  GLuint last_frame_texture_ = 0;
  uint64_t last_frame_number_ = 0;
  using DrawKey = std::tuple<uint64_t, GLuint, uint64_t, std::string, int, int, bool, std::string, bool, double, double, int, GLuint, uint64_t, float, GLuint, uint64_t, float, float>;
  std::optional<DrawKey> draw_key_;
  std::string last_frame_occurrence_;
  void activate();
  void swap(const Surface* artwork);
  void quad(GLuint texture, Bounds rect, float opacity = 1);
  void artwork_quad(const Surface& surface, Bounds rect, float opacity);
  void draw_pointer(bool pressed);
  void draw_loading(uint64_t ticks);
  std::string loading_occurrence_;
  uint64_t loading_started_ = 0;
  void upload_ui(UIRaster raster, const std::string& key);
  void ui(const std::string& text, const std::string& qr, bool full);
};
}
