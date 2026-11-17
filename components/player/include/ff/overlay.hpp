#pragma once
#include "ff/json.hpp"

namespace ff {
struct OverlayPanel { std::string text, qr; };
// Admission is separate from painting: malformed credentials or unknown setup
// states fail the command while the previous usable panel remains visible.
void validate_overlay(const Json& overlay);
OverlayPanel overlay_panel(const Json& overlay);
}
