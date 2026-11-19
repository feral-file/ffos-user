#pragma once
#include "ff/json.hpp"
#include <functional>
#include <set>

namespace ff {
struct InputGeometry {
  int width = 1, height = 1;
  int left = 0, top = 0, view_width = 1, view_height = 1;
  bool operator==(const InputGeometry&) const = default;
};
struct PhysicalKey { int virtual_key = 0, native_key = 0; };
// USB HID keyboard-page usage -> Chromium VK and Linux XKB native keycode.
// Text is a separate UTF-8 event and never inferred from physical position.
PhysicalKey physical_key(int usage);
// Typed browser-engine raw key payload. It never synthesizes text.
Json browser_key(const Json& event);

class InputState {
 public:
  explicit InputState(std::function<void(const Json&)> sink) : sink_(std::move(sink)) {}
  void target(std::string occurrence, InputGeometry geometry);
  void dispatch(const Json& event);
  void release();
  void flush();
  double x() const { return x_; }
  double y() const { return y_; }
  uint32_t buttons() const { return buttons_; }
 private:
  std::function<void(const Json&)> sink_;
  std::string target_;
  InputGeometry geometry_;
  double x_ = 0, y_ = 0;
  uint32_t buttons_ = 0;
  std::set<int> keys_;
  bool pending_move_ = false;
  bool positioned_ = false;
  void emit(Json event);
};
}
