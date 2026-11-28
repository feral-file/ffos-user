#pragma once
#include <string>
namespace ff {
void sweep_browser_profiles(const std::string& directory);
void retire_browser_profile(const std::string& directory);
}  // namespace ff
