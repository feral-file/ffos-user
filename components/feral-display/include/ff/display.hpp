#pragma once
#include "ff/json.hpp"

namespace ff {
// Resolve admitted display layers for one occurrence, in DP-1 precedence order.
Json resolve_display(const Json& settings, const Json& defaults, const Json& item,
                     const Json& adjustment = Json::object());
bool allows_display_override(const Json& display, const std::string& field);

struct Bounds { int x, y, width, height; };
// DP1 percentage padding is vh vertically and vw horizontally. Keep at least
// one pixel available even for an authored margin larger than half the output.
bool hard_pixel_edges(const Json& display,int source_width,int source_height,int width,int height);
Bounds display_bounds(const Json& display,int width,int height);
// Resolve CSS literal colors once into opaque sRGB over the device's black
// base. Artist documents remain unchanged; every backend gets the same color.
std::string display_color(const std::string& css);
}
