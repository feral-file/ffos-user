#include "ff/playback.hpp"
#include <filesystem>
#include <iostream>

// Same OS boot and SQLite file, distinct host lifetimes. A short presentation
// on each attempt must not replenish the retry budget for a known failing work.
int main() {
  const auto dir = std::filesystem::temp_directory_path() / ("ff-restarted-artwork-" + ff::uuid());
  std::filesystem::create_directory(dir);
  const ff::Json items = {{{"id", "stalls"}, {"source", "https://example.org/stalls"}, {"duration", 240}},
                          {{"id", "next"}, {"source", "https://example.org/next"}, {"duration", 240}}};
  int result = 1;
  try {
    int64_t clock = 1000;
    {
      ff::Store store((dir / "state.sqlite").string());
      ff::Playback playback(store);
      playback.system_boot("same-boot");
      playback.tick(clock, clock);
      playback.command("playback.replace", {{"playlist", {{"items", items}}}});
      const auto id = playback.desired()->id;
      playback.committed(id);
      playback.presented(id);
    }
    for (int host = 0; host < 5; ++host) {
      ff::Store store((dir / "state.sqlite").string());
      ff::Playback playback(store);
      playback.system_boot("same-boot");
      playback.tick(clock, clock);
      auto selected = playback.desired();
      if (!selected) throw std::runtime_error("Restart lost the active playlist");
      if (host == 0 && selected->index != 0)
        throw std::runtime_error("Ordinary restart skipped a healthy work");
      if (selected->item.at("id") == "next") {
        playback.committed(selected->id);
        playback.presented(selected->id);
        if (!playback.visible() || playback.visible()->index != 1)
          throw std::runtime_error("Successor did not present");
        result = 0;
        break;
      }
      const auto id = selected->id;
      playback.committed(id);
      playback.presented(id);
      clock += 5000;
      playback.tick(clock, clock);
      playback.failed(id, "Artwork renderer did not answer its layout health probe for 5 seconds");
      // Give recovery a full backoff before losing the host, as on a kiosk
      // restart. The test accepts advancement on this tick or on reopening.
      clock += 5000;
      playback.tick(clock, clock);
    }
    if (result) std::cerr << "Five host lifetimes reselected the same failing work\n";
  } catch (const std::exception& e) {
    std::cerr << e.what() << '\n';
  }
  std::filesystem::remove_all(dir);
  return result;
}
