#pragma once
#include <memory>
#include <SDL.h>

namespace ff {
// A transparent Cage toplevel exists only while device chrome needs pixels
// above a directly presented browser. The artist's EGL surface stays mapped.
class CageOverlay {
 public:
  CageOverlay(SDL_Window* owner,int width,int height);
  ~CageOverlay();
  void begin();
  void submit();
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}
