#include "ff/json.hpp"
#include <chrono>
#include <fstream>
#include <iomanip>
#include <set>
#include <sstream>
#include <vector>

namespace ff {
void require(bool condition, std::string_view code, std::string_view detail) {
  if (!condition) throw Error(std::string(code), std::string(detail));
}
Json parse(std::string_view text) {
  require(!text.empty() && text.size() <= max_frame, "frame_size", "JSON body exceeds bounds");
  require(!text.starts_with("\xef\xbb\xbf"),"invalid_json","JSON byte-order marks are not accepted");
  std::vector<std::set<std::string>> keys;
  try {
    return Json::parse(text, [&](int depth, Json::parse_event_t event, Json& parsed) {
      require(depth <= 64, "invalid_json", "JSON nesting exceeds 64 levels");
      if (event == Json::parse_event_t::object_start) keys.emplace_back();
      if (event == Json::parse_event_t::key)
        require(keys.back().insert(parsed.get<std::string>()).second, "invalid_json", "Duplicate object member");
      if (event == Json::parse_event_t::object_end) keys.pop_back();
      return true;
    });
  } catch (const Json::exception&) {
    throw Error("invalid_json", "Invalid UTF-8 JSON document");
  }
}
void fields(const Json& value, std::initializer_list<std::string_view> allowed) {
  require(value.is_object(), "invalid_params", "Expected an object");
  for (auto it = value.begin(); it != value.end(); ++it) {
    bool known = false;
    for (auto name : allowed) known |= name == it.key();
    require(known, "invalid_params", "Unknown field: " + it.key());
  }
}
std::string string_field(const Json& value, const char* name, size_t maximum) {
  require(value.contains(name) && value.at(name).is_string(), "invalid_params", std::string("Missing string: ") + name);
  auto s = value.at(name).get<std::string>();
  require(!s.empty() && s.size() <= maximum && s.find('\0') == std::string::npos, "invalid_params", std::string("Invalid string: ") + name);
  return s;
}
std::string uuid() {
  unsigned char bytes[16];
  std::ifstream random("/dev/urandom", std::ios::binary);
  random.read(reinterpret_cast<char*>(bytes), sizeof bytes);
  require(random.good(), "entropy_unavailable", "Cannot obtain runtime identity");
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  std::ostringstream out;
  out << std::hex << std::setfill('0');
  for (int i = 0; i < 16; ++i) {
    if (i == 4 || i == 6 || i == 8 || i == 10) out << '-';
    out << std::setw(2) << static_cast<int>(bytes[i]);
  }
  return out.str();
}
int64_t wall_ms() {
  return std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::system_clock::now().time_since_epoch()).count();
}
}
