#pragma once

namespace ff {
struct FadeOpacity {
  float outgoing, incoming;
  bool complete;
};

// CSS `ease` is cubic-bezier(.25, .1, .25, 1). Find the curve parameter
// for elapsed time before sampling opacity on the Y axis.
inline float artwork_ease(double time) {
  if (time <= 0) return 0;
  if (time >= 1) return 1;
  const auto cubic = [](double t, double p1, double p2) {
    const double u = 1 - t;
    return 3 * u * u * t * p1 + 3 * u * t * t * p2 + t * t * t;
  };
  double low = 0, high = 1;
  for (int i = 0; i < 20; ++i) {
    const double t = (low + high) / 2;
    if (cubic(t, .25, .25) < time) low = t;
    else high = t;
  }
  return static_cast<float>(cubic((low + high) / 2, .1, 1));
}

inline FadeOpacity artwork_fade(double elapsed_ms, double duration_ms, bool sequential) {
  if (duration_ms <= 0) return {0, 1, true};
  const double time = elapsed_ms / duration_ms;
  if (!sequential) {
    const auto incoming = artwork_ease(time);
    return {1 - incoming, incoming, time >= 1};
  }
  return {1 - artwork_ease(time), artwork_ease(time - 1), time >= 2};
}
}  // namespace ff
