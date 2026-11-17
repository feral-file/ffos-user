#include "ff/input.hpp"
#include <algorithm>
#include <cmath>

namespace ff {
PhysicalKey physical_key(int usage) {
  // evdev positions from Linux input-event-codes.h; Chromium's Linux native
  // keycode is XKB (evdev + 8), verified against the pinned CEF/Chromium source.
  if (usage >= 4 && usage <= 29) {
    static constexpr int evdev[] = {30,48,46,32,18,33,34,35,23,36,37,38,50,49,24,25,16,19,31,20,22,47,17,45,21,44};
    return {65 + usage - 4, evdev[usage - 4] + 8};
  }
  if (usage >= 30 && usage <= 38) return {49 + usage - 30, 2 + usage - 30 + 8};
  if (usage == 39) return {48, 11 + 8};
  if (usage >= 58 && usage <= 69) return {112 + usage - 58, (usage <= 67 ? 59 + usage - 58 : 87 + usage - 68) + 8};
  struct Entry { int usage, vk, evdev; };
  static constexpr Entry table[] = {
    {40,13,28},{41,27,1},{42,8,14},{43,9,15},{44,32,57},
    {45,189,12},{46,187,13},{47,219,26},{48,221,27},{49,220,43},
    {51,186,39},{52,222,40},{53,192,41},{54,188,51},{55,190,52},{56,191,53},
    {57,20,58},{70,44,99},{71,145,70},{72,19,119},{73,45,110},{74,36,102},
    {75,33,104},{76,46,111},{77,35,107},{78,34,109},{79,39,106},{80,37,105},
    {81,40,108},{82,38,103},{83,144,69},{84,111,98},{85,106,55},{86,109,74},
    {87,107,78},{88,13,96},{89,97,79},{90,98,80},{91,99,81},{92,100,75},
    {93,101,76},{94,102,77},{95,103,71},{96,104,72},{97,105,73},{98,96,82},
    {99,110,83},{100,226,86},{101,93,127},
    {224,17,29},{225,16,42},{226,18,56},{227,91,125},
    {228,17,97},{229,16,54},{230,18,100},{231,92,126}
  };
  for (auto key : table) if (key.usage == usage) return {key.vk,key.evdev + 8};
  throw Error("unsupported", "Unsupported USB HID keyboard usage");
}
void InputState::target(std::string occurrence, InputGeometry geometry) {
  require(geometry.width > 0 && geometry.height > 0 && geometry.view_width > 0 && geometry.view_height > 0,
    "invalid_params", "Invalid input geometry");
  if (occurrence != target_) { release(); target_ = std::move(occurrence); }
  else if (geometry != geometry_) flush();
  geometry_ = geometry;
  x_ = std::clamp(x_,0.0,static_cast<double>(geometry.width - 1));
  y_ = std::clamp(y_,0.0,static_cast<double>(geometry.height - 1));
}
void InputState::emit(Json event) {
  if (target_.empty()) return;
  event["target"] = target_;
  event["x"] = static_cast<int>(std::floor(x_)) - geometry_.left;
  event["y"] = static_cast<int>(std::floor(y_)) - geometry_.top;
  event["buttons"] = buttons_;
  event["shift"] = keys_.contains(225) || keys_.contains(229);
  event["control"] = keys_.contains(224) || keys_.contains(228);
  event["alt"] = keys_.contains(226) || keys_.contains(230);
  event["meta"] = keys_.contains(227) || keys_.contains(231);
  event["outside"] = x_ < geometry_.left || y_ < geometry_.top || x_ >= geometry_.left + geometry_.view_width || y_ >= geometry_.top + geometry_.view_height;
  sink_(event);
}
void InputState::release() {
  flush();
  for (int button=1;button<=3;++button) {
    uint32_t mask=1U<<(button-1);
    if (buttons_ & mask) { buttons_ &= ~mask; emit({{"kind","button"},{"button",button},{"down",false},{"clicks",1}}); }
  }
  while (!keys_.empty()) {
    auto key=*keys_.begin();keys_.erase(key);
    emit({{"kind","key"},{"usage",key},{"down",false},{"repeat",false}});
  }
  emit({{"kind","blur"}});
}
void InputState::flush() {
  if (pending_move_) { pending_move_ = false; emit({{"kind","move"}}); }
}
void InputState::dispatch(const Json& event) {
  auto kind=string_field(event,"kind",32);
  if (kind=="releaseAll") { release();return; }
  if (target_.empty()) return;
  if (kind=="move") {
    double x=event.contains("dx")?x_+event.at("dx").get<double>():event.value("x",x_);
    double y=event.contains("dy")?y_+event.at("dy").get<double>():event.value("y",y_);
    require(std::isfinite(x)&&std::isfinite(y),"invalid_params","Invalid pointer position");
    x_=std::clamp(x,0.0,static_cast<double>(geometry_.width-1));
    y_=std::clamp(y,0.0,static_cast<double>(geometry_.height-1));
    pending_move_=true;
    return;
  }
  // Coalesce only contiguous motion. Never move a key/button transition past
  // the pointer state at which it was received.
  flush();
  if (kind=="button") {
    int button=event.value("button",1),clicks=event.value("clicks",1);
    require(button>=1&&button<=3&&clicks>=1&&clicks<=3,"invalid_params","Invalid button transition");
    auto mask=1U<<(button-1);bool down=event.at("down").get<bool>();
    if (down) buttons_|=mask;else buttons_&=~mask;
    emit({{"kind",kind},{"button",button},{"down",down},{"clicks",clicks}});
  } else if (kind=="key") {
    int usage=event.at("usage").get<int>();physical_key(usage);
    bool down=event.at("down").get<bool>(),repeat=down&&keys_.contains(usage);
    if(down)keys_.insert(usage);else keys_.erase(usage);
    emit({{"kind",kind},{"usage",usage},{"down",down},{"repeat",repeat}});
  } else if (kind=="text" || kind=="wheel") emit(event);
  else throw Error("invalid_params","Unknown input kind");
}
}
