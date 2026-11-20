#pragma once
#include "ff/playback.hpp"
#include <cstdint>
#include <string>
#include <vector>

namespace ff {
// Device UI is rasterized only when content/geometry changes. Artwork never
// passes through Cairo, and the countdown is drawn separately by the GPU.
struct UIRaster {
  int width = 0, height = 0;
  int timer_x = 0, timer_y = 0, timer_width = 0, timer_height = 0;
  std::vector<uint8_t> rgba;
};
struct PointerRaster {
  UIRaster image;
  int hotspot;
};
PointerRaster render_pointer(int size, bool pressed);
void load_device_fonts(const std::string& directory);
UIRaster render_panel(const Json& overlay, int width, int height);
UIRaster render_message(const std::string& text, int width, int height, bool full = true);
bool has_tombstone(const Occurrence& occurrence);
UIRaster render_tombstone(const Occurrence& occurrence, int width, int height, bool timed);
}
