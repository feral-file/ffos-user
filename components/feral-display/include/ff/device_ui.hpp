#pragma once
#include "ff/playback.hpp"
#include <cstdint>
#include <string>
#include <vector>

namespace ff {
// Device UI is rasterized only when content/geometry changes. Artwork never
// passes through Cairo.
struct UIRaster {
  int width = 0, height = 0;
  std::vector<uint8_t> rgba;
};
struct PointerRaster {
  UIRaster image;
  int hotspot;
};
PointerRaster render_pointer(int size, bool pressed);
void load_device_fonts(const std::string& directory);
UIRaster render_panel(const Json& overlay, int width, int height);
UIRaster render_loading_label();
UIRaster render_message(const std::string& text, int width, int height, bool full = true);
bool has_tombstone(const Occurrence& occurrence);
UIRaster render_title_card(const Occurrence& occurrence, int width, int height);

class TitleCardClock {
 public:
  void select(const std::string& playlist, uint64_t now);
  const std::optional<Occurrence>& artwork(uint64_t now, const std::optional<Occurrence>& selected,
                                          const std::optional<Occurrence>& presented) const;
  bool visible(uint64_t now, bool titled, bool work_ready, bool can_show=true);
 private:
  static constexpr uint64_t hour_ms = 3600000;
  std::string playlist_;
  uint64_t start_ = 0, hour_ = 0;
  std::optional<uint64_t> readable_, covered_;
  bool finished_ = false;
};
}
