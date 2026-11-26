#pragma once

#include <filesystem>
#include <string>

namespace ff {
inline bool valid_engine_digest(const std::string& digest) {
  return digest.size() == 64 && digest.find_first_not_of("0123456789abcdef") == digest.npos;
}

// Only the administrator can install a verified rootfs into this directory.
inline std::filesystem::path capsule_engine_root(const std::string& digest) {
  return std::filesystem::path("/var/lib/ff-player-native/engines") / digest;
}
}
