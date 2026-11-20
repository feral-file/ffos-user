#pragma once
#include "ff/display.hpp"
#include <GLES3/gl3.h>
#include <chrono>
#include <memory>
#include <string>

namespace ff {
// One glycin decoder per artwork. The frame texture stays resident while a
// still image is paused or the host redraws device UI.
class Raster {
 public:
  Raster(const std::string& source, int fd, const std::string& agent);
  ~Raster();
  bool update(bool paused, bool loop);
  void draw(GLuint target, int width, int height, const Json& display, bool direct);
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}
