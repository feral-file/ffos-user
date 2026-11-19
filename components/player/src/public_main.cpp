#include <sys/stat.h>

#include <algorithm>
#include <csignal>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <map>
#include <thread>

#include "ff/media_client.hpp"
#include "ff/profile_gc.hpp"
#include "ff/public_browser.hpp"
#include "ff/server.hpp"
namespace {
volatile std::sig_atomic_t stopping = 0;
void stop(int) { stopping = 1; }
}  // namespace
int main(int argc, char** argv) {
  umask(0077);
  std::signal(SIGTERM, stop);
  std::signal(SIGINT, stop);
  std::signal(SIGPIPE, SIG_IGN);
  std::map<std::string, std::string> options;
  for (int i = 1; i < argc; ++i) {
    std::string arg = argv[i];
    auto equals = arg.find('=');
    options[arg.substr(0, equals)] = equals == std::string::npos ? "" : arg.substr(equals + 1);
  }
  auto state_path = options["--state-dir"], socket = options["--socket"];
  if (state_path.empty() || socket.empty()) {
    std::cerr << "Required --state-dir=PATH --socket=PATH\n";
    return 2;
  }
  try {
    std::filesystem::create_directories(state_path);
    ff::Store store(state_path + "/state.sqlite");
    ff::sweep_browser_profiles(state_path + "/browser");
    ff::Playback playback(store);
    std::ifstream boot_file("/proc/sys/kernel/random/boot_id");
    std::string boot_id;
    std::getline(boot_file, boot_id);
    ff::require(!boot_id.empty(), "boot_identity", "Cannot read boot identity");
    playback.system_boot(boot_id);
    ff::Protocol protocol(playback);
    ff::Server server(socket, protocol);
    auto root = std::filesystem::canonical("/proc/self/exe").parent_path();
    auto worker = (root / "media/ff-media-launch").string(),
         launcher = (root / "ff-browser-launch").string();
    auto resources = options["--renderer-dir"];
    if (resources.empty()) resources = "/usr/share/ff-player-native/renderers";
    std::ifstream engine_manifest(std::filesystem::path(resources).parent_path() /
                                  "dependencies.json");
    if (engine_manifest) {
      auto manifest = ff::Json::parse(engine_manifest);
      auto engine = manifest.at("chromium");
      playback.engine_status({{"product", "Chrome/" + engine.at("version").get<std::string>()},
                              {"userAgent", engine.at("observedUserAgent")},
                              {"identity", "packaged"}});
    }
    ff::Presentation presentation(
        false, options.contains("--diagnostic-presentation-timing"),
        (std::filesystem::path(resources).parent_path() / "fonts").string());
    std::map<std::string, std::unique_ptr<ff::PublicBrowser>> clients;
    std::map<std::string, std::unique_ptr<ff::MediaClient>> media;
    ff::InputState input_state([&](const ff::Json& event) {
      if (auto it = clients.find(event.at("target").get<std::string>()); it != clients.end())
        it->second->input(event);
    });
    bool input_ready = false;
    auto sync_input = [&] {
      ff::InputGeometry geometry;
      geometry.width = presentation.width();
      geometry.height = presentation.height();
      std::string target;
      if (playback.visible() && !playback.composition_pending()) {
        auto bounds = presentation.bounds(playback.visible()->display);
        auto it = clients.find(playback.visible()->id);
        if (it == clients.end() || (it->second->frames.shown.width == bounds.width &&
                                    it->second->frames.shown.height == bounds.height))
          target = playback.visible()->id;
        geometry.left = bounds.x;
        geometry.top = bounds.y;
        geometry.view_width = bounds.width;
        geometry.view_height = bounds.height;
      }
      input_ready = !target.empty();
      input_state.target(target, geometry);
      presentation.pointer_position(input_state.x(), input_state.y());
    };
    auto input = [&](const ff::Json& event) {
      sync_input();
      ff::require(input_ready || event.value("kind", "") == "releaseAll", "input_blocked",
                  "Artwork input geometry is changing");
      input_state.dispatch(event);
      if (event.value("kind", "") == "move" || event.value("kind", "") == "button")
        presentation.pointer(input_state.x(), input_state.y());
    };
    playback.set_input_handler(input);
    playback.set_seek_handler([&](const std::string& id, double seconds) {
      auto it = media.find(id);
      ff::require(it != media.end(), "unsupported",
                  "This artwork has no native media seek control");
      ff::require(it->second->status.contains("seekable") && it->second->status["seekable"] == true,
                  "unsupported", "This media source is not seekable");
      it->second->seek(seconds);
    });
    const auto cache_socket = (std::filesystem::path(socket).parent_path() / "cache.sock").string();
    struct SourceResolution {
      std::string id;
      std::shared_ptr<ff::CacheInbox> inbox;
      std::optional<std::string> mode;
      std::chrono::steady_clock::time_point started;
    } resolution;
    auto resolve_source = [&](const ff::Occurrence& occurrence) -> std::optional<std::string> {
      if (!playback.online() ||
          occurrence.item.at("source").get<std::string>().starts_with("data:"))
        return "";
      if (resolution.id != occurrence.id) {
        if (resolution.inbox) resolution.inbox->active = false;
        resolution = {occurrence.id, std::make_shared<ff::CacheInbox>(), std::nullopt,
                      std::chrono::steady_clock::now()};
        if (!ff::cache_request(resolution.inbox, cache_socket, "", occurrence.id,
                               {{"source", occurrence.item.at("source")},
                                {"url", occurrence.item.at("source")},
                                {"method", "GET"},
                                {"headers", ff::Json::object()}},
                               false, true))
          throw ff::Error("cache_backpressure", "Cache mode request queue is full");
      }
      if (!resolution.mode) {
        std::lock_guard lock(resolution.inbox->mutex);
        if (!resolution.inbox->replies.empty()) {
          const auto& reply = resolution.inbox->replies.front();
          // Never infer live permission from an unavailable broker: the same
          // source may have a manual, network-isolated capture. In particular,
          // controld restart/startup order must not change its trust boundary.
          ff::require(reply.error.empty(), "cache_policy_unavailable",
                      "Cannot resolve artwork cache mode: " + reply.error);
          auto mode = reply.metadata.value("replayMode", "");
          ff::require(mode.empty() || mode == "isolated" || mode == "mixed", "cache_protocol",
                      "Unknown artwork cache mode");
          resolution.mode = (mode == "isolated" || mode == "mixed") ? mode : "";
        } else if (std::chrono::steady_clock::now() - resolution.started > std::chrono::seconds(6))
          throw ff::Error("cache_policy_unavailable", "Timed out resolving artwork cache mode");
        if (resolution.mode) resolution.inbox->active = false;
      }
      return resolution.mode;
    };
    std::string backdrop_id;
    bool backdrop_attempted = false;
    int64_t backdrop_fade_at = 0;
    auto frame = [&] {
      auto now = std::chrono::steady_clock::now().time_since_epoch();
      const auto now_ms = std::chrono::duration_cast<std::chrono::milliseconds>(now).count();
      playback.tick(now_ms, ff::wall_ms());
      server.poll();
      const bool setup_visible = !playback.overlay().is_null() &&
                                 playback.overlay().value("owner", "") == "setup" &&
                                 !playback.visible() && !playback.thermal_hold();
      if (setup_visible) {
        backdrop_fade_at = 0;
        if (!backdrop_attempted) {
          backdrop_attempted = true;
          try {
            ff::Occurrence decoration;
            decoration.id = "setup-" + ff::uuid();
            decoration.item = {{"source", "ff-render://bundle/setup-artwork/index.html"}};
            decoration.display = {{"margin", 0}};
            backdrop_id = decoration.id;
            clients.emplace(
                backdrop_id,
                std::make_unique<ff::PublicBrowser>(
                    playback, presentation, decoration, launcher,
                    state_path + "/browser/" + ff::uuid(),
                    (std::filesystem::path(socket).parent_path() / "cache.sock").string(),
                    resources, true));
          } catch (const std::exception& e) {
            backdrop_id.clear();
            std::cerr << "Setup backdrop unavailable: " << e.what() << '\n';
          }
        }
      } else {
        if (!backdrop_id.empty() && !backdrop_fade_at) backdrop_fade_at = now_ms;
        if (backdrop_id.empty() || (backdrop_fade_at && now_ms - backdrop_fade_at >= 650)) {
          backdrop_id.clear();
          backdrop_attempted = false;
        }
      }

      if (!presentation.events([&](const ff::Json& event) {
            try {
              if (event.value("kind", "") == "releaseAll" ||
                  (playback.overlay().is_null() &&
                   (!playback.desired() || playback.desired()->stage != "failed")))
                input(event);
            } catch (const std::exception& e) {
              std::cerr << "input: " << e.what() << '\n';
            }
          })) {
        stopping = 1;
        return;
      }
      input_state.flush();
      if (auto desired = playback.desired();
          desired && playback.connectivity_known() && desired->stage != "failed" &&
          !clients.contains(desired->id) && !media.contains(desired->id)) {
        try {
          auto cache_mode = resolve_source(*desired);
          if (cache_mode && ff::native_media(desired->item))
            media.emplace(desired->id,
                          std::make_unique<ff::MediaClient>(playback, presentation, *desired,
                                                            worker, cache_socket, *cache_mode));
          else if (cache_mode)
            clients.emplace(
                desired->id,
                std::make_unique<ff::PublicBrowser>(
                    playback, presentation, *desired, launcher,
                    state_path + "/browser/" + desired->id,
                    (std::filesystem::path(socket).parent_path() / "cache.sock").string(),
                    resources, false, options.contains("--diagnostic-engine-debugger"),
                    *cache_mode));
        } catch (const std::exception& e) {
          playback.failed(desired->id, e.what());
        }
      }
      for (auto& [id, client] : clients) client->poll();
      if (auto desired = playback.desired(); desired) {
        auto it = clients.find(desired->id);
        if (it != clients.end() && it->second->needs_live_restart())
          playback.command("playback.refresh", ff::Json::object());
      }
      for (auto it = clients.begin(); it != clients.end();) {
        auto& client = *it->second;
        if (!client.resolved_native_mime.empty() && playback.desired() &&
            playback.desired()->id == it->first) {
          auto rendering = *playback.desired();
          rendering.item["mimeType"] = client.resolved_native_mime;
          playback.release_input();
          auto id = it->first;
          auto cached_source = client.cache_mode();
          it = clients.erase(it);
          try {
            media.emplace(
                id, std::make_unique<ff::MediaClient>(playback, presentation, rendering, worker,
                                                      cache_socket, cached_source));
          } catch (const std::exception& e) {
            playback.failed(id, e.what());
          }
        } else
          ++it;
      }
      for (auto& [id, client] : media) client->poll();
      ff::Surface* surface = nullptr;
      std::string commit;
      if (auto desired = playback.desired(); desired) {
        auto it = clients.find(desired->id);
        if (it != clients.end() && it->second->has_ready_frame() && desired->stage != "failed") {
          surface = &it->second->frames.latest();
          commit = desired->id;
        }
      }
      if (auto desired = playback.desired(); desired) {
        auto it = media.find(desired->id);
        if (it != media.end() && it->second->ready && desired->stage != "failed") {
          surface = &it->second->frames.latest();
          commit = desired->id;
        }
      }
      if (!surface && playback.visible()) {
        auto it = clients.find(playback.visible()->id);
        if (it != clients.end()) surface = &it->second->frames.latest();
      }
      if (!surface && playback.visible()) {
        auto it = media.find(playback.visible()->id);
        if (it != media.end()) surface = &it->second->frames.latest();
      }
      // Presentation identity advances only after a host-owned frame exists.
      // A swap is composition evidence, never proof of artist correctness or HDMI output.
      const auto* drawing = !commit.empty() ? &*playback.desired()
                                            : (playback.visible() ? &*playback.visible() : nullptr);
      if (!commit.empty()) playback.frame_ready(commit);
      std::optional<ff::Occurrence> retained_composition;
      float backdrop_opacity =
          setup_visible      ? 1.f
          : backdrop_fade_at ? std::clamp(1.f - float(now_ms - backdrop_fade_at) / 650, 0.f, 1.f)
                             : 0.f;
      bool configuration_ready =
          drawing && surface && surface->display_revision == drawing->display_revision;
      // Covered changes cannot replace the texture associated with recorded
      // composition. A later superseding change may need that exact frame
      // when the overlay disappears before the new configuration is ready.
      if (drawing && (!playback.overlay().is_null() || backdrop_opacity > 0) &&
          !playback.composition().is_null() &&
          playback.composition().value("occurrence", "") == drawing->id &&
          playback.composition().value("displayRevision", uint64_t{0}) != drawing->display_revision)
        configuration_ready = false;
      if (drawing && !configuration_ready && !playback.composition().is_null() &&
          playback.composition().value("occurrence", "") == drawing->id) {
        if (auto it = clients.find(drawing->id); it != clients.end())
          surface = &it->second->frames.shown;
        else if (auto it = media.find(drawing->id); it != media.end())
          surface = &it->second->frames.shown;
        retained_composition = *drawing;
        retained_composition->display = playback.composition()["display"];
        drawing = &*retained_composition;
      }
      ff::Surface* backdrop = nullptr;
      if (auto it = clients.find(backdrop_id);
          it != clients.end() && it->second->has_ready_frame() && !it->second->failed())
        backdrop = &it->second->frames.latest();
      const bool submitted =
          presentation.draw(surface, drawing, playback, backdrop, backdrop_opacity);
      if (submitted && !commit.empty() && configuration_ready && playback.overlay().is_null() &&
          backdrop_opacity == 0)
        playback.presented(commit);
      if (submitted && drawing && configuration_ready && playback.overlay().is_null() &&
          backdrop_opacity == 0)
        playback.composed(drawing->id, surface->display_revision);
      if (submitted && drawing && configuration_ready) {
        if (auto it = clients.find(drawing->id); it != clients.end()) it->second->did_present();
        if (auto it = media.find(drawing->id); it != media.end()) it->second->did_present();
      }
      if (submitted && backdrop) {
        auto it = clients.find(backdrop_id);
        if (it != clients.end()) it->second->did_present();
      }
      sync_input();
      for (auto it = clients.begin(); it != clients.end();) {
        bool keep = (playback.desired() && playback.desired()->stage != "failed" &&
                     playback.desired()->id == it->first) ||
                    (playback.visible() && playback.visible()->id == it->first) ||
                    (it->first == backdrop_id && !it->second->failed());
        if (!keep)
          it = clients.erase(it);
        else
          ++it;
      }
      for (auto it = media.begin(); it != media.end();) {
        bool keep = (playback.desired() && playback.desired()->stage != "failed" &&
                     playback.desired()->id == it->first) ||
                    (playback.visible() && playback.visible()->id == it->first);
        if (!keep)
          it = media.erase(it);
        else
          ++it;
      }
      ff::reap_media_workers();
      ff::reap_public_browsers();
    };
    while (!stopping) {
      frame();
      std::this_thread::sleep_for(std::chrono::milliseconds(4));
    }
    playback.release_input();
    clients.clear();
    media.clear();
    auto metrics = presentation.metrics();
    if (options.contains("--diagnostic-presentation-timing")) {
      std::ofstream output(options["--diagnostic-presentation-timing"]);
      output << metrics.dump() << '\n';
    }
    metrics["waylandPresentation"].erase("changedTimestampsNs");
    std::cerr << "presentation_metrics " << metrics.dump() << '\n';
    return 0;
  } catch (const std::exception& e) {
    std::cerr << "ff-player-native: " << e.what() << '\n';
    return 1;
  }
}
