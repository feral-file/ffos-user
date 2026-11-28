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
Json browser_key(const Json& event) {
  const int usage=event.at("usage").get<int>();auto physical=physical_key(usage);
  std::string code,key;const bool shift=event.value("shift",false);
  if(usage>=4&&usage<=29){code="Key"+std::string(1,char('A'+usage-4));key=std::string(1,char((shift?'A':'a')+usage-4));}
  else if(usage>=30&&usage<=39){char value=usage==39?'0':char('1'+usage-30);code="Digit"+std::string(1,value);key=std::string(1,shift?std::string("!@#$%^&*()")[usage-30]:value);}
  else if(usage>=58&&usage<=69)code=key="F"+std::to_string(usage-57);
  else {
    struct Key {int usage;const char* code;const char* key;const char* shifted;};
    static const Key keys[]={
      {40,"Enter","Enter","Enter"},{41,"Escape","Escape","Escape"},{42,"Backspace","Backspace","Backspace"},{43,"Tab","Tab","Tab"},{44,"Space"," "," "},
      {45,"Minus","-","_"},{46,"Equal","=","+"},{47,"BracketLeft","[","{"},{48,"BracketRight","]","}"},{49,"Backslash","\\","|"},{51,"Semicolon",";",":"},{52,"Quote","'","\""},{53,"Backquote","`","~"},{54,"Comma",",","<"},{55,"Period",".",">"},{56,"Slash","/","?"},
      {57,"CapsLock","CapsLock","CapsLock"},{70,"PrintScreen","PrintScreen","PrintScreen"},{71,"ScrollLock","ScrollLock","ScrollLock"},{72,"Pause","Pause","Pause"},{73,"Insert","Insert","Insert"},{74,"Home","Home","Home"},{75,"PageUp","PageUp","PageUp"},{76,"Delete","Delete","Delete"},{77,"End","End","End"},{78,"PageDown","PageDown","PageDown"},
      {79,"ArrowRight","ArrowRight","ArrowRight"},{80,"ArrowLeft","ArrowLeft","ArrowLeft"},{81,"ArrowDown","ArrowDown","ArrowDown"},{82,"ArrowUp","ArrowUp","ArrowUp"},
      {83,"NumLock","NumLock","NumLock"},{84,"NumpadDivide","/","/"},{85,"NumpadMultiply","*","*"},{86,"NumpadSubtract","-","-"},{87,"NumpadAdd","+","+"},{88,"NumpadEnter","Enter","Enter"},
      {89,"Numpad1","1","1"},{90,"Numpad2","2","2"},{91,"Numpad3","3","3"},{92,"Numpad4","4","4"},{93,"Numpad5","5","5"},{94,"Numpad6","6","6"},{95,"Numpad7","7","7"},{96,"Numpad8","8","8"},{97,"Numpad9","9","9"},{98,"Numpad0","0","0"},{99,"NumpadDecimal",".","."},{100,"IntlBackslash","\\","|"},{101,"ContextMenu","ContextMenu","ContextMenu"},
      {224,"ControlLeft","Control","Control"},{225,"ShiftLeft","Shift","Shift"},{226,"AltLeft","Alt","Alt"},{227,"MetaLeft","Meta","Meta"},{228,"ControlRight","Control","Control"},{229,"ShiftRight","Shift","Shift"},{230,"AltRight","Alt","Alt"},{231,"MetaRight","Meta","Meta"}
    };
    for(const auto& entry:keys)if(entry.usage==usage){code=entry.code;key=shift?entry.shifted:entry.key;break;}
  }
  require(!code.empty(),"unsupported","No browser physical key mapping");
  int modifiers=(event.value("alt",false)?1:0)|(event.value("control",false)?2:0)|(event.value("meta",false)?4:0)|(shift?8:0);
  return {{"type",event.at("down").get<bool>()?"rawKeyDown":"keyUp"},{"windowsVirtualKeyCode",physical.virtual_key},{"nativeVirtualKeyCode",physical.native_key},{"code",code},{"key",key},{"modifiers",modifiers},{"autoRepeat",event.value("repeat",false)},{"isKeypad",usage>=83&&usage<=99},{"location",usage>=224?(usage>=228?2:1):usage>=83&&usage<=99?3:0}};
}
void InputState::target(std::string occurrence, InputGeometry geometry) {
  require(geometry.width > 0 && geometry.height > 0 && geometry.view_width > 0 && geometry.view_height > 0,
    "invalid_params", "Invalid input geometry");
  const bool changed = occurrence != target_;
  if (changed) { release(); target_ = std::move(occurrence); }
  else if (geometry != geometry_) flush();
  geometry_ = geometry;
  if (!positioned_ && !target_.empty()) {
    x_ = geometry.width / 2.0; y_ = geometry.height / 2.0; positioned_ = true;
  }
  x_ = std::clamp(x_,0.0,static_cast<double>(geometry.width - 1));
  y_ = std::clamp(y_,0.0,static_cast<double>(geometry.height - 1));
  // A newly presented view needs the current hover position even on a stationary kiosk.
  if (changed && !target_.empty()) pending_move_ = true;
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
void InputState::observe_pointer(double x,double y) {
  x_=std::clamp(x,0.0,static_cast<double>(geometry_.width-1));
  y_=std::clamp(y,0.0,static_cast<double>(geometry_.height-1));
}
void InputState::dispatch(const Json& event) {
  auto kind=string_field(event,"kind",32);
  if (kind=="releaseAll") { release();return; }
  if (target_.empty()) return;
  if (kind=="move") {
    double scale=event.contains("referenceWidth")?geometry_.width/event.at("referenceWidth").get<double>():1.0;
    double x=event.contains("dx")?x_+event.at("dx").get<double>()*scale:event.value("x",x_);
    double y=event.contains("dy")?y_+event.at("dy").get<double>()*scale:event.value("y",y_);
    require(std::isfinite(x)&&std::isfinite(y),"invalid_params","Invalid pointer position");
    observe_pointer(x,y);
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
