#include <sys/stat.h>

#include <algorithm>
#include <csignal>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <map>
#include <thread>
#include <utility>

#include "ff/artwork_fade.hpp"
#include "ff/media_client.hpp"
#include "ff/profile_gc.hpp"
#include "ff/public_browser.hpp"
#include "ff/renderer_path.hpp"
#include "ff/server.hpp"
namespace {
volatile std::sig_atomic_t stopping = 0;
void stop(int) { stopping = 1; }
}  // namespace
extern "C" int ff_host(int argc, char** argv) {
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
    auto worker = (root / "ff-player-native").string(),
         launcher = (root / "ff-player-native").string();
    auto resources = options["--renderer-dir"];
    if (resources.empty()) resources = ff::renderer_directory(root).string();
    std::string browser_backend = "public-chromium";
    std::ifstream engine_manifest(std::filesystem::path(resources).parent_path() /
                                  "dependencies.json");
    if (engine_manifest) {
      auto manifest = ff::Json::parse(engine_manifest);
      if (!manifest.contains("chromium") && manifest.contains("cefViews")) browser_backend = "cef-views";
      const auto& engine = manifest.at(browser_backend == "cef-views" ? "cefViews" : "chromium");
      playback.engine_status({{"product", "Chrome/" + engine.at("version").get<std::string>()},
                              {"userAgent", engine.at("observedUserAgent")},
                              {"identity", "packaged"}});
    }
    ff::Presentation presentation(
        false, options.contains("--diagnostic-presentation-timing"),
        (std::filesystem::path(resources).parent_path() / "fonts").string());
    std::map<std::string, std::unique_ptr<ff::PublicBrowser>> clients;
    ff::ArtworkCache artwork_cache((root / "ff-player-native").string());
    std::map<std::string, std::unique_ptr<ff::MediaClient>> media;
    struct Transition {
      std::string outgoing, incoming;
      ff::Json outgoing_display;
      ff::Json policy, update_outcome;
      std::optional<int64_t> started;
      uint64_t incoming_revision;
      double duration;
      bool sequential;
      bool staged;
      bool settling = false;
    };
    std::optional<Transition> transition;
    auto frames_for = [&](const std::string& id) -> ff::FramePair* {
      if (auto it = clients.find(id); it != clients.end()) return &it->second->frames;
      if (auto it = media.find(id); it != media.end()) return &it->second->frames;
      return nullptr;
    };
    std::string composed_browser_id;
    bool stage_fading = false;
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
      if (playback.visible() && !stage_fading && !playback.composition_pending()) {
        auto bounds = presentation.bounds(playback.visible()->display);
        auto it = clients.find(playback.visible()->id);
        if (it == clients.end() || it->second->direct() || (it->second->frames.shown.width == bounds.width &&
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
        presentation.pointer(input_state.x(), input_state.y(), input_state.buttons() != 0);
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
    std::string bundle_root;
    struct SourceResolution {
      std::string id;
      std::shared_ptr<ff::CacheInbox> inbox;
      std::optional<std::string> mode;
      std::chrono::steady_clock::time_point started;
      ff::Json streams;
    } resolution;
    auto resolve_source = [&](const ff::Occurrence& occurrence) -> std::optional<std::string> {
      if (occurrence.item.at("source").get<std::string>().starts_with("data:") ||
          !ff::local_artwork_name(occurrence.item.at("source")).empty())
        return "";
      if (resolution.id != occurrence.id) {
        if (resolution.inbox) resolution.inbox->active = false;
        resolution = {occurrence.id, std::make_shared<ff::CacheInbox>(), std::nullopt,
                      std::chrono::steady_clock::now()};
        if (!ff::cache_request(resolution.inbox, bundle_root, "", occurrence.id,
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
          resolution.streams = reply.metadata.value("streams", ff::Json::object());
          resolution.mode = (mode == "isolated" || mode == "mixed") ? mode : "";
        } else if (std::chrono::steady_clock::now() - resolution.started > std::chrono::seconds(6))
          throw ff::Error("cache_policy_unavailable", "Timed out resolving artwork cache mode");
        if (resolution.mode) resolution.inbox->active = false;
      }
      return resolution.mode;
    };
    std::string backdrop_id;
    auto applied_permissions = playback.global_policy();
    bool backdrop_attempted = false;
    int64_t backdrop_fade_at = 0;
    auto frame = [&] {
      auto now = std::chrono::steady_clock::now().time_since_epoch();
      const auto now_ms = std::chrono::duration_cast<std::chrono::milliseconds>(now).count();
      playback.tick(now_ms, ff::wall_ms());
      server.poll();
      bundle_root = playback.artwork_sources().value("bundleRoot", "/home/feralfile/.cache/offline-artworks");
      artwork_cache.maintain(playback.global_policy().at("cache"));
      if (applied_permissions.at("cache") != playback.global_policy().at("cache"))
        applied_permissions["cache"] = playback.global_policy().at("cache");
      if (applied_permissions != playback.global_policy()) {
        // Even outgoing, preloaded and backdrop workers must lose the old grants.
        transition.reset();
        stage_fading = false;
        clients.clear();
        media.clear();
        backdrop_id.clear();
        backdrop_attempted = false;
        applied_permissions = playback.global_policy();
      }
      if (transition &&
          (!playback.desired() || playback.desired()->id != transition->incoming ||
           playback.desired()->display_revision != transition->incoming_revision ||
           playback.desired()->stage == "failed" || !playback.visible() ||
           !playback.overlay().is_null() || playback.service_failure() ||
           playback.state()["policy"] != transition->policy ||
           playback.state().value("updateOutcome", ff::Json(nullptr)) != transition->update_outcome)) {
        // Retained outgoing pixels are also subject to policy and refresh.
        transition.reset();
        stage_fading = false;
      }
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
                    bundle_root,
                    resources, browser_backend, true));
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
          artwork_cache.stop_warming();
          auto cache_mode = resolve_source(*desired);
          if (cache_mode && ff::native_media(desired->item))
            media.emplace(desired->id,
                          std::make_unique<ff::MediaClient>(playback, presentation, *desired,
                                                            worker, bundle_root, *cache_mode, &artwork_cache));
          else if (cache_mode)
            clients.emplace(
                desired->id,
                std::make_unique<ff::PublicBrowser>(
                    playback, presentation, *desired, launcher,
                    state_path + "/browser/" + ff::uuid(),
                    bundle_root,
                    resources, browser_backend, false, options.contains("--diagnostic-engine-debugger"),
                    *cache_mode, options.contains("--diagnostic-browser-render-ahead"), composed_browser_id!=desired->id,
                    cache_mode->empty() ? ff::Json::object() : resolution.streams));
        } catch (const std::exception& e) {
          playback.failed(desired->id, e.what());
        }
      }
      for (auto& [id, client] : clients) {
        client->poll();
        if(auto sample=std::exchange(client->physical_pointer,std::nullopt);
           sample && playback.visible() && playback.visible()->id==id && playback.overlay().is_null()){
          // Cage already delivered this event to Chromium. Only update the device glyph.
          input_state.observe_pointer(sample->x,sample->y);
          presentation.pointer(input_state.x(),input_state.y(),sample->clicked);
        }
      }
      if (auto desired = playback.desired(); desired) {
        auto it = clients.find(desired->id);
        if(it!=clients.end() && it->second->needs_compositor()){
          // Reopen this occurrence once with host composition for a new layout
          // or when the compositor cannot confirm the direct presentation.
          composed_browser_id=desired->id;
          playback.release_input();
          clients.erase(it);
        } else if (it != clients.end() && it->second->needs_live_restart())
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
                                                      bundle_root, cached_source, &artwork_cache));
          } catch (const std::exception& e) {
            playback.failed(id, e.what());
          }
        } else
          ++it;
      }
      const bool stage_requested = transition && transition->staged && !transition->settling;
      for (auto& [id, client] : media) {
        const bool hold = transition && transition->started && id == transition->outgoing;
        const auto* stage = stage_requested && id == transition->incoming
                                ? &transition->outgoing_display
                                : nullptr;
        client->poll(hold, stage,!transition);
        if(client->pointer_motion){
          sync_input();
          if(input_ready && playback.overlay().is_null()){
            input(*client->pointer_motion);
            if(client->pointer_clicked) presentation.pointer(input_state.x(), input_state.y(), true);
          }
          client->pointer_clicked=false;
          client->pointer_motion.reset();
        }
      }
      // The outgoing renderer can fail during polling. Its staged successor
      // must wait for a normal frame in that case rather than reuse the matte.
      if (transition && !playback.visible()) {
        transition.reset();
        stage_fading = false;
      }
      // A direct browser's output is admitted by Cage's presentation event.
      // The host never manufactures a texture or a scanout acknowledgement.
      if(auto desired=playback.desired();desired && desired->stage!="failed"){
        auto it=clients.find(desired->id);
        if(it!=clients.end() && it->second->direct() && it->second->has_ready_frame() &&
           playback.overlay().is_null() && !playback.service_failure()){
          playback.frame_ready(desired->id);
          playback.presented(desired->id);
          playback.composed(desired->id,desired->display_revision);
        }
      }
      ff::Surface* surface = nullptr;
      std::string commit;
      if (auto desired = playback.desired(); desired) {
        auto it = clients.find(desired->id);
        if (it != clients.end() && !it->second->direct() && it->second->has_ready_frame() && desired->stage != "failed") {
          surface = &it->second->frames.latest();
          commit = desired->id;
        }
      }
      if (auto desired = playback.desired(); desired) {
        auto it = media.find(desired->id);
        if (it != media.end() && it->second->ready && desired->stage != "failed" &&
            (!stage_requested || transition)) {
          surface = &it->second->frames.latest();
          commit = desired->id;
        }
      }
      if (!surface && playback.visible()) {
        auto it = clients.find(playback.visible()->id);
        if (it != clients.end() && !it->second->direct()) surface = &it->second->frames.latest();
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
      if (stage_requested && transition && !commit.empty() && commit == transition->incoming)
        configuration_ready = true;  // MediaClient checked the private staged revision.
      // Covered changes cannot replace the texture associated with recorded
      // composition. A later superseding change may need that exact frame
      // when the overlay disappears before the new configuration is ready.
      if (drawing && (!playback.overlay().is_null() || backdrop_opacity > 0 || playback.service_failure()) &&
          !playback.composition().is_null() &&
          playback.composition().value("occurrence", "") == drawing->id &&
          playback.composition_pending())
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
      ff::ArtworkBlend blend;
      bool transition_complete = false;
      bool apply_final_configuration = false;
      bool staged_composition = false;
      const bool unobscured =
          playback.overlay().is_null() && backdrop_opacity == 0 && !playback.service_failure();
      if (transition && (!unobscured || !playback.visible())) {
        transition.reset();
        stage_fading = false;
      }
      if (!transition && !commit.empty() && configuration_ready && unobscured &&
          playback.visible() && playback.visible()->id != commit &&
          !playback.composition_pending()) {
        const auto& old = *playback.visible();
        const auto* frames = frames_for(old.id);
        const auto duration = playback.state()["settings"].value("transitionMs", 650.0);
        const bool cage_outgoing=clients.contains(old.id) && clients.at(old.id)->direct();
        const bool retained=frames && frames->shown.texture && frames->shown.display_revision==old.display_revision;
        if (duration > 0 && (cage_outgoing || retained)) {
          // SVG, GLB and software documents follow the old embedded-view
          // fade-through-background path. Only native media overlaps.
          const bool sequential = clients.contains(old.id) || clients.contains(commit);
          const auto stage_differs = [&](const char* key) {
            return old.display.at(key) != drawing->display.at(key);
          };
          const bool staged = !sequential && (stage_differs("scaling") || stage_differs("margin"));
          transition = Transition{old.id, commit, old.display, playback.state()["policy"],
                                  playback.state().value("updateOutcome", ff::Json(nullptr)),
                                  std::nullopt, drawing->display_revision, duration, sequential,
                                  staged};
        }
      }
      if (transition && transition->settling) {
        transition_complete = configuration_ready && !commit.empty();
      } else if (transition) {
        auto* outgoing = frames_for(transition->outgoing);
        if (!transition->started && !commit.empty() && configuration_ready &&
            (!transition->staged || stage_requested))
          transition->started = now_ms;
        const auto opacity = transition->started
                                 ? ff::artwork_fade(now_ms - *transition->started,
                                                    transition->duration, transition->sequential)
                                 : ff::FadeOpacity{1, 0, false};
        const bool drew_incoming = configuration_ready && !commit.empty();
        transition_complete = opacity.complete && drew_incoming && !transition->staged;
        apply_final_configuration = opacity.complete && drew_incoming && transition->staged;
        if (opacity.incoming == 0 && outgoing && playback.visible()->id == transition->outgoing) {
          // The outgoing item still owns the screen, input and presentation
          // acknowledgement until some incoming artwork is actually drawn.
          drawing = &*playback.visible();
          auto& newest = outgoing->latest();
          configuration_ready = newest.display_revision == drawing->display_revision;
          surface = configuration_ready ? &newest : &outgoing->shown;
          blend.stage = &transition->outgoing_display;
          blend.opacity = opacity.outgoing;
          commit.clear();
        } else if (drew_incoming && (!opacity.complete || apply_final_configuration)) {
          blend.opacity = opacity.incoming;
          if (!transition->sequential && outgoing) {
            // Both layers use the previous stage until the outgoing slot is
            // gone. The retained shown texture survives retiring its media.
            blend.stage = &transition->outgoing_display;
            blend.outgoing = &outgoing->shown;
            blend.outgoing_opacity = opacity.outgoing;
            staged_composition = true;
          }
        }
      }
      ff::Surface* backdrop = nullptr;
      if (auto it = clients.find(backdrop_id);
          it != clients.end() && it->second->has_ready_frame() && !it->second->failed())
        backdrop = &it->second->frames.latest();
      const bool submitted =
          presentation.draw(surface, drawing, playback, backdrop, backdrop_opacity, blend,
                            drawing && clients.contains(drawing->id) && clients.at(drawing->id)->direct());
      if (submitted && !commit.empty() && configuration_ready && unobscured && blend.opacity > 0)
        playback.presented(commit);
      if (submitted && drawing && configuration_ready && unobscured && blend.opacity > 0) {
        if (staged_composition) {
          auto observed = drawing->display;
          for (auto key : {"margin", "scaling", "background"}) observed[key] = blend.stage->at(key);
          playback.composed(drawing->id, drawing->display_revision, &observed);
        } else {
          playback.composed(drawing->id, drawing->display_revision);
        }
      }
      if (submitted && drawing && configuration_ready && blend.opacity > 0) {
        if (auto it = clients.find(drawing->id); it != clients.end() && !it->second->direct()) it->second->did_present();
        if (auto it = media.find(drawing->id); it != media.end()) it->second->did_present();
      }
      if (submitted && backdrop) {
        auto it = clients.find(backdrop_id);
        if (it != clients.end()) it->second->did_present();
      }
      if (submitted) {
        stage_fading = staged_composition || blend.opacity == 0;
        if (apply_final_configuration) transition->settling = true;
        if (transition_complete) transition.reset();
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
                    (playback.visible() && playback.visible()->id == it->first) ||
                    (transition && !transition->sequential && !transition->settling &&
                     transition->outgoing == it->first);
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
