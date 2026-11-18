#pragma once
#include <cstdint>
#include <utility>

namespace ff {
struct Surface {
  uint32_t texture = 0;
  int width = 0, height = 0;
  uint64_t frames = 0, display_revision = 0;
  bool accelerated = false;
};
// Two host-owned GPU textures, no additional copy. Capture never overwrites the
// last submitted frame. A superseding configuration can discard pending frames
// without changing the pixels associated with the last composition status.
struct FramePair {
  Surface capture, shown;
  uint64_t count = 0;
  bool pending = false;
  void captured() { capture.frames=++count;pending=true; }
  Surface& latest() { return pending?capture:shown; }
  const Surface& latest() const { return pending?capture:shown; }
  void submitted() { if(pending){std::swap(capture,shown);pending=false;} }
};
}
