#include "ff/server.hpp"
#include <chrono>
#include <csignal>
#include <iostream>
#include <thread>
#include <sys/stat.h>

namespace { volatile std::sig_atomic_t stopping = 0; void stop(int) { stopping = 1; } }
int main(int argc, char** argv) {
  if (argc != 3) { std::cerr << "Usage: ff-player-core STATE.sqlite SOCKET\nProtocol/state test host only; no rendering backend.\n"; return 2; }
  umask(0077);
  std::signal(SIGTERM, stop); std::signal(SIGINT, stop); std::signal(SIGPIPE, SIG_IGN);
  try {
    ff::Store store(argv[1]); ff::Playback playback(store); ff::Protocol protocol(playback); ff::Server server(argv[2], protocol);
    while (!stopping) {
      auto now = std::chrono::steady_clock::now().time_since_epoch();
      playback.tick(std::chrono::duration_cast<std::chrono::milliseconds>(now).count(), ff::wall_ms());
      server.poll(); std::this_thread::sleep_for(std::chrono::milliseconds(5));
    }
  } catch (const std::exception& e) { std::cerr << "ff-player-core: " << e.what() << '\n'; return 1; }
}
