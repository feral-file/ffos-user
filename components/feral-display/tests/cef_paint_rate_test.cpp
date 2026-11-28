#include "ff/cef_paint_rate.hpp"
#include <stdexcept>
using namespace std::chrono;

struct Trial {
  ff::CefPaintRate clock;
  milliseconds elapsed{};
  int frame(int interval_ms, int copy_ms) {
    elapsed += milliseconds(interval_ms);
    return clock.sample(ff::CefPaintRate::Clock::time_point{} + elapsed, milliseconds(copy_ms));
  }
};
void check(bool ok) { if (!ok) throw std::runtime_error("unexpected CEF capture rate"); }

int main() {
  Trial trial;
  for (int i=0; i<100; ++i) check(trial.frame(33, 5) == 60);
  check(trial.frame(50, 45) == 30);
  check(trial.frame(250, 200) == 30);
  check(trial.frame(300, 70) == 15);
  // Drain outstanding GPU work before judging the lower rate.
  for (int i=0; i<20; ++i) check(trial.frame(100, 90) == 15);
  while (trial.clock.rate() == 15 && trial.elapsed < seconds(15)) trial.frame(67, 10);
  check(trial.clock.rate() == 16);
  check(trial.frame(250, 250) == 15);
  for (int i=0; i<400; ++i) check(trial.frame(67, 4) == 15);

  // A cheap workload can recover all the way to 60 Hz on the same page.
  for (int i=0; i<5000; ++i) trial.frame(1000 / trial.clock.rate(), 3);
  check(trial.clock.rate() == 60);

  // Capturing faster does not help if the compositor still displays 15 Hz.
  ff::CefPaintRate display;
  milliseconds time{100};
  auto presented_sample = [&](int interval, int copy) {
    time += milliseconds(interval);
    return display.sample(ff::CefPaintRate::Clock::time_point{} + time, milliseconds(copy),
                          static_cast<uint64_t>(time.count()) * 15 / 1000);
  };
  check(presented_sample(50, 50) == 30);
  check(presented_sample(600, 70) == 15);
  for (int i=0; i<100 && display.rate()==15; ++i) presented_sample(67, 4);
  check(display.rate() == 16);
  for (int i=0; i<100 && display.rate()==16; ++i) presented_sample(62, 4);
  check(display.rate() == 15);

  Trial busy;
  check(busy.frame(50, 45) == 30);
  check(busy.frame(600, 70) == 15);
  for (int i=0; i<200; ++i) check(busy.frame(67, 54) == 15);

  Trial high;
  check(high.frame(50, 50) == 30);
  for (int i=0; i<400 && high.clock.rate()<40; ++i) high.frame(1000/high.clock.rate(), 3);
  check(high.clock.rate() == 40);
  // This 2x-budget copy backs off normally after settling, clearing the
  // upward probe. A stale probe would restore the old limit under overload.
  for (int i=0; i<25; ++i) high.frame(25, 2);
  check(high.frame(60, 60) == 20);
  for (int i=0; i<60; ++i) check(high.frame(120, 110) <= 20);
}
