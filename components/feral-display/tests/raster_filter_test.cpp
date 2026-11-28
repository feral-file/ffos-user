#include "ff/raster_filter.hpp"
#include <mpv/client.h>
#include <chrono>
#include <iostream>
#include <memory>
#include <stdexcept>

// Exercise libmpv's graph parser and FFmpeg's integer rounding with real frames.
void check_size(int width, int height, int expected_width, int expected_height,
                int texture_limit, bool pixelated) {
  std::unique_ptr<mpv_handle, decltype(&mpv_terminate_destroy)> player(mpv_create(), mpv_terminate_destroy);
  if (!player) throw std::runtime_error("Cannot allocate libmpv");
  auto set = [&](const char* key, const std::string& value) {
    if (mpv_set_option_string(player.get(), key, value.c_str()) < 0)
      throw std::runtime_error(std::string("Cannot set ") + key);
  };
  set("config", "no");
  set("vo", "null");
  set("ao", "null");
  set("pause", "yes");
  const auto filter = ff::raster_upload_filter(width, height, texture_limit, pixelated);
  if (filter.empty() != (width == expected_width && height == expected_height))
    throw std::runtime_error("Unexpected raster filter selection");
  if (!filter.empty()) set("vf", filter);
  if (mpv_initialize(player.get()) < 0) throw std::runtime_error("Cannot initialize libmpv");
  const std::string image = "memory://P5\n" + std::to_string(width) + " " +
                            std::to_string(height) + "\n255\n" + std::string(width * height, 'X');
  const char* load[] = {"loadfile", image.c_str(), nullptr};
  if (mpv_command(player.get(), load) < 0) throw std::runtime_error("Cannot load test image");
  const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(5);
  while (std::chrono::steady_clock::now() < deadline) {
    auto event = mpv_wait_event(player.get(), 0.1);
    if (event->event_id == MPV_EVENT_END_FILE) break;
    int64_t w = 0, h = 0;
    if (mpv_get_property(player.get(),"video-out-params/w", MPV_FORMAT_INT64,&w) >= 0 &&
        mpv_get_property(player.get(),"video-out-params/h", MPV_FORMAT_INT64,&h) >= 0 &&
        w == expected_width && h == expected_height) return;
  }
  throw std::runtime_error("Unexpected raster output for " + std::to_string(width) + "x" + std::to_string(height));
}

int main() {
  try {
    check_size(401, 254, 401, 254, 16384, false);
    check_size(8192, 1, 8192, 1, 16384, false);
    check_size(10000, 1, 4096, 1, 16384, false);
    check_size(1, 10000, 1, 4096, 16384, true);
    check_size(800, 200, 512, 128, 512, true);
  } catch (const std::exception& error) {
    std::cerr << error.what() << '\n';
    return 1;
  }
}
