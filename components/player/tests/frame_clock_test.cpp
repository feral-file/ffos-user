#include "ff/frame_clock.hpp"
#include <stdexcept>
using namespace std::chrono;
int main() {
  ff::FrameClock clock;
  auto start=ff::FrameClock::Clock::time_point{};
  auto expect=[&](int64_t ns,bool wanted){if(clock.due(start+nanoseconds(ns))!=wanted)throw std::runtime_error("frame clock phase/catch-up regression");};
  expect(0,true);expect(0,false);expect(16000000,false);
  expect(20000000,true); // A 4 ms polling loop first sees the 16.67 ms slot late.
  expect(32000000,false);expect(34000000,true); // Do not drift to 40 ms.
  expect(500000010,true);expect(500000010,false); // Skip a long GPU stall once.
  expect(516666677,true);expect(516666678,false);
}
