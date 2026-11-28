#pragma once
#include <algorithm>
#include <cstdint>
#include <string>

namespace ff {
// Only an oversized source needs a CPU rendition before upload to GL. Keep
// its 4K detail across rotations and margin changes throughout the occurrence.
inline std::string raster_upload_filter(int64_t width, int64_t height, int texture_limit, bool pixelated) {
  const int64_t longest = std::max(width, height);
  if (width <= 0 || height <= 0 || longest <= std::min(texture_limit, 8192)) return {};
  const int64_t target = std::min(texture_limit, 4096);
  const auto scale = [&](int64_t dimension) {
    return std::to_string(std::max<int64_t>(1, (dimension * target + longest / 2) / longest));
  };
  return "lavfi=[scale=w=" + scale(width) + ":h=" + scale(height) +
         (pixelated ? ":flags=neighbor]" : "]");
}
}
