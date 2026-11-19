#pragma once
#include <cstdint>
#include <utility>

namespace ff {
struct DmaFrame {
  struct Rect {int x=0,y=0,width=0,height=0;} visible;
  int width=0,height=0,count=0; uint32_t format=0; uint64_t modifier=0;
  struct Plane { int fd=-1; uint32_t stride=0; uint64_t offset=0; } planes[4];
};
struct Surface {
  uint32_t texture = 0;
  int width = 0, height = 0;
  uint64_t frames = 0, display_revision = 0;
  bool accelerated = false;
  void* copy_fence = nullptr;
  int64_t copy_started_ns = 0;
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
