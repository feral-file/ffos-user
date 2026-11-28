#pragma once
#include <filesystem>

namespace ff {
inline std::filesystem::path renderer_directory(const std::filesystem::path& binary_directory) {
  // Isolated FF1 installs keep renderer manifests and fonts beside their own
  // engine. A build-tree executable can still use the system installation.
  auto packaged = binary_directory.parent_path().parent_path() / "share/feral-display/renderers";
  if (std::filesystem::is_directory(packaged)) return packaged;
  return "/usr/share/feral-display/renderers";
}
}  // namespace ff
