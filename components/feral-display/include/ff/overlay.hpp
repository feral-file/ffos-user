#pragma once
#include "ff/json.hpp"
#include <vector>

namespace ff {
enum class PanelStyle { title, subtitle, compact, step, code, note };
struct PanelLine {
  PanelStyle style;
  std::string markup;
};
struct OverlayPanel {
  std::vector<PanelLine> before_qr, after_qr;
  std::string qr;
};
// Admission is separate from painting: malformed credentials or unknown setup
// states fail the command while the previous usable panel remains visible.
void validate_overlay(const Json& overlay);
OverlayPanel overlay_panel(const Json& overlay);
}
