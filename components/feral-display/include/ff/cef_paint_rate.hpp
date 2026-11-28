#pragma once
#include <algorithm>
#include <chrono>
#include <cstdint>
#include <optional>

namespace ff {
class CefPaintRate {
 public:
  using Clock = std::chrono::steady_clock;
  int rate() const { return rate_; }

  int sample(Clock::time_point now, Clock::duration copy,
             std::optional<uint64_t> presented = std::nullopt) {
    using namespace std::chrono;
    const auto budget = duration<double>(1.0 / rate_);
    if (probe_from_ && copy > budget * 3) {
      next_probe_ = now + seconds(30);
      return change(now, probe_from_);
    }
    if (now < settled_) return rate_;
    if (rate_ >= 30 && copy > budget * 1.5) {
      return change(now, rate_ / 2);
    }
    if (!samples_) {
      start_ = now;
      first_presented_ = presented;
    }
    if (!presented) first_presented_.reset();
    ++samples_;
    work_ += copy;
    const auto elapsed = now - start_;
    if (elapsed < seconds(2)) return rate_;
    // Count compositor-confirmed changes when this view owns the output.
    // Several captures can be coalesced into one visible update.
    const auto frames = first_presented_ && presented && *presented >= *first_presented_
        ? *presented - *first_presented_ : samples_ - 1;
    const double delivered = frames / duration<double>(elapsed).count();
    const bool overloaded = delivered < rate_ * 0.75 && work_ > elapsed * 0.65;
    // A saturated renderer can hit its current limit with almost no
    // headroom. Probing then queues GPU work and misses display refreshes.
    const bool spare = work_ < elapsed * 0.65;
    samples_ = 0;
    work_ = {};

    // Keep a higher producer limit only if it buys actual capture throughput.
    // At the GPU saturation point another Hz can create a queue lasting many
    // refreshes. Let it drain before sampling at the previous limit again.
    if (probe_from_) {
      const bool useful = delivered > before_probe_ + 0.6;
      const int previous = probe_from_;
      probe_from_ = 0;
      if (!useful && (overloaded || delivered < rate_ * 0.95) && before_probe_ >= previous * 0.8) {
        next_probe_ = now + seconds(30);
        return change(now, previous);
      }
    }
    if (overloaded && rate_ > 10) return change(now, std::max(10, rate_ >= 30 ? rate_ / 2 : rate_ - 1));
    if (spare && now >= next_probe_ && rate_ < 60) {
      probe_from_ = rate_;
      before_probe_ = delivered;
      return change(now, std::min(60, rate_ + (rate_ < 30 ? 1 : 5)));
    }
    return rate_;
  }

 private:
  int change(Clock::time_point now, int next) {
    using namespace std::chrono;
    if (next < rate_) probe_from_ = 0;
    rate_ = next;
    samples_ = 0;
    work_ = {};
    settled_ = now + (next < 30 ? milliseconds(2500) : milliseconds(500));
    return rate_;
  }
  int rate_ = 60, probe_from_ = 0;
  unsigned samples_ = 0;
  double before_probe_ = 0;
  std::optional<uint64_t> first_presented_;
  Clock::duration work_{};
  Clock::time_point start_{}, settled_{}, next_probe_{};
};
}
