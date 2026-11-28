#pragma once
#include <algorithm>
#include <chrono>

namespace ff {
class CefHealthProbe {
 public:
  using Clock = std::chrono::steady_clock;
  enum class Action { none, send, write_timeout, reply_timeout };

  explicit CefHealthProbe(Clock::time_point now = Clock::now()) : last_poll_(now), next_(now + timeout) {}

  // Called after draining DevTools replies. A long interval with no pipe
  // service (e.g. eglSwapBuffers) is only one observation of silence.
  Action poll(Clock::time_point now, bool loaded) {
    const auto gap = now - last_poll_;
    last_poll_ = now;
    if (phase_ == Phase::idle) {
      if (!loaded || now < next_) return Action::none;
      next_ = now + timeout;
      phase_ = Phase::queued;
      observed_ = {};
      return Action::send;
    }
    observed_ += std::min<Clock::duration>(gap, std::chrono::milliseconds(100));
    if (observed_ < timeout) return Action::none;
    return phase_ == Phase::queued ? Action::write_timeout : Action::reply_timeout;
  }

  void written(Clock::time_point now) {
    phase_ = Phase::awaiting_reply;
    observed_ = {};
    last_poll_ = now;
  }
  void answered() { phase_ = Phase::idle; }

 private:
  enum class Phase { idle, queued, awaiting_reply };
  static constexpr auto timeout = std::chrono::seconds(5);
  Phase phase_ = Phase::idle;
  Clock::duration observed_{};
  Clock::time_point last_poll_, next_;
};
}  // namespace ff
