#pragma once
#include "ff/json.hpp"

namespace ff {
enum class SketchStartup { finished, awaiting_instance, pending, failed };

// Snapshot booleans only. Reading arbitrary page objects by value can invoke
// serialization hooks. CDP also forbids side effects when reading these flags.
inline constexpr char sketch_startup_expression[] = R"JS([
  globalThis.p5?.instance?._setupDone === false,
  typeof globalThis.p5?.instance?._preloadCount === 'number' && globalThis.p5.instance._preloadCount < 0,
  globalThis.p5?.instance?.frameCount === 0,
  typeof globalThis.p5 === 'function' && globalThis.p5.instance == null
])JS";

inline SketchStartup sketch_startup_state(const Json& flags) {
  if (!flags.is_array() || flags.size() != 4) return SketchStartup::finished;
  if (flags[3] == true) return SketchStartup::awaiting_instance;
  if (flags[0] != true || flags[2] != true) return SketchStartup::finished;
  // p5 v1 enters setup at zero pending preloads. A late completion can
  // decrement the count below zero after setup threw. Its equality check
  // will never re-enter setup, and no first draw was scheduled.
  return flags[1] == true ? SketchStartup::failed : SketchStartup::pending;
}
}  // namespace ff
