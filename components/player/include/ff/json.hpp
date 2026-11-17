#pragma once
#include <json.hpp>
#include <stdexcept>
#include <string>
#include <string_view>

namespace ff {
using Json = nlohmann::json;
inline constexpr size_t max_frame = 4 * 1024 * 1024;
struct Error : std::runtime_error {
  std::string code;
  Error(std::string code, std::string detail) : std::runtime_error(std::move(detail)), code(std::move(code)) {}
};
Json parse(std::string_view text);
std::string uuid();
int64_t wall_ms();
void require(bool condition, std::string_view code, std::string_view detail);
void fields(const Json& value, std::initializer_list<std::string_view> allowed);
std::string string_field(const Json& value, const char* name, size_t maximum = 4096);
}
