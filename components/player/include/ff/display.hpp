#pragma once
#include "ff/json.hpp"

namespace ff {
struct Bounds { int x, y, width, height; };
// DP1 percentage padding is vh vertically and vw horizontally. Keep at least
// one pixel available even for an authored margin larger than half the output.
Bounds display_bounds(const Json& display,int width,int height);
// Resolve CSS literal colors once into opaque sRGB over the device's black
// base. Artist documents remain unchanged; every backend gets the same color.
std::string display_color(const std::string& css);
}
