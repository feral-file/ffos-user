#pragma once
#include "ff/playback.hpp"
#include "include/cef_render_handler.h"
#include <SDL.h>
#include <GLES3/gl3.h>
#include <EGL/egl.h>

namespace ff {
struct Surface {
  GLuint texture = 0;
  int width = 0, height = 0;
  uint64_t frames = 0;
  bool accelerated = false;
};
struct Bounds { int x, y, width, height; };
class Presentation {
 public:
  explicit Presentation(bool diagnostic_cpu);
  ~Presentation();
  bool accelerated(Surface& surface, const CefAcceleratedPaintInfo& info);
  void software(Surface& surface, const void* data, int width, int height);
  void destroy(Surface& surface);
  void draw(const Surface* artwork, const Occurrence* occurrence, const Playback& playback);
  bool events(const std::function<void(const Json&)>& input);
  Bounds bounds(const Json& display) const;
  int width() const { return width_; }
  int height() const { return height_; }
  void pointer(double x, double y) { pointer_x_ = x; pointer_y_ = y; pointer_active_ = SDL_GetTicks64(); }
  void pointer_position(double x, double y) { pointer_x_ = x; pointer_y_ = y; }
  Json metrics() const;
 private:
  SDL_Window* window_ = nullptr;
  SDL_GLContext context_ = nullptr;
  int width_ = 0, height_ = 0;
  bool diagnostic_cpu_;
  GLuint program_ = 0, ui_ = 0;
  std::string ui_key_;
  std::string label_occurrence_;
  uint64_t label_started_ = 0;
  int ui_width_ = 0, ui_height_ = 0;
  double pointer_x_ = 0, pointer_y_ = 0;
  uint64_t pointer_active_ = 0, gpu_copies_ = 0, cpu_copies_ = 0, swaps_ = 0;
  uint64_t producer_busy_frames_ = 0;
  double copy_ms_ = 0;
  void quad(GLuint texture, Bounds rect, float opacity = 1);
  void ui(const std::string& text, const std::string& qr, bool full);
};
}
