#include "ff/sketch_startup.hpp"
#include <stdexcept>

int main() {
  using ff::SketchStartup;
  auto expect = [](ff::Json flags, SketchStartup wanted) {
    if (ff::sketch_startup_state(flags) != wanted) throw std::runtime_error("Wrong sketch startup decision");
  };
  expect({true, true, true, false}, SketchStartup::failed);   // Late map, setup aborted, no draw.
  expect({true, false, true, false}, SketchStartup::pending); // Preload still has a chance to finish.
  expect({false, true, true, false}, SketchStartup::finished); // Completed setup, including noLoop.
  expect({true, true, false, false}, SketchStartup::finished); // The sketch already drew.
  expect({false, false, false, false}, SketchStartup::finished); // An ordinary page.
  expect({false, false, false, true}, SketchStartup::awaiting_instance); // Deferred global init.
  expect(nullptr, SketchStartup::finished); // Unsupported or inaccessible state.
}
