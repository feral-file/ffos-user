#include "ff/cef_health_probe.hpp"
#include <stdexcept>

using namespace std::chrono;
using Probe = ff::CefHealthProbe;
using Action = Probe::Action;

void expect(Action actual, Action expected) {
  if (actual != expected) throw std::runtime_error("Unexpected layout health probe decision");
}

int main() {
  const Probe::Clock::time_point start{};
  Probe slow(start);
  expect(slow.poll(start + 5s, true), Action::send);
  expect(slow.poll(start + 13s, true), Action::none);  // swap before write
  slow.written(start + 13s);
  expect(slow.poll(start + 21186ms, true), Action::none);  // observed 8.186s swap
  expect(slow.poll(start + 21210ms, true), Action::none);
  slow.answered();
  expect(slow.poll(start + 21220ms, true), Action::send);

  Probe hung(start);
  expect(hung.poll(start + 5s, true), Action::send);
  hung.written(start + 5s);
  // A missed polling window cannot exempt a renderer from subsequent checks.
  expect(hung.poll(start + 15s, true), Action::none);
  for (int i = 1; i < 49; ++i)
    expect(hung.poll(start + 15s + i * 100ms, true), Action::none);
  expect(hung.poll(start + 19900ms, true), Action::reply_timeout);

  Probe blocked(start);
  expect(blocked.poll(start + 5s, true), Action::send);
  for (int i = 1; i < 50; ++i)
    expect(blocked.poll(start + 5s + i * 100ms, true), Action::none);
  expect(blocked.poll(start + 10s, true), Action::write_timeout);
}
