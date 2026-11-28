#pragma once
#include <chrono>

namespace ff {
// Keep a 60 Hz phase without accumulating polling delay. A late caller skips
// missed slots; it never emits a burst to catch up after a GPU wait.
class FrameClock {
 public:
  using Clock = std::chrono::steady_clock;
  bool due(Clock::time_point now) {
    if (started_ && now < next_) return false;
    if (!started_) {started_=true;next_=now+period_;}
    else next_ += ((now-next_)/period_+1)*period_;
    return true;
  }
 private:
  static constexpr auto period_ = std::chrono::nanoseconds(16666667);
  Clock::time_point next_{};
  bool started_ = false;
};
}
