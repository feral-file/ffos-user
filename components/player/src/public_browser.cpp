#include "ff/public_browser.hpp"
#include "ff/capsule_engine.hpp"
#include "ff/sketch_startup.hpp"

#include <fcntl.h>
#include <glib.h>
#include "ff/artwork_cache.hpp"
#include <linux/input-event-codes.h>
#include <spawn.h>
#include <sys/wait.h>
#include <sys/socket.h>
#include <unistd.h>

#include <algorithm>
#include <csignal>
#include <ctime>
#include <cstring>
#include <strings.h>
#include <filesystem>
#include <fstream>
#include <iostream>

#include "ff/display.hpp"
#include "ff/media_kind.hpp"
#include "ff/profile_gc.hpp"
extern char** environ;
namespace ff {
namespace {
struct Retired {
  pid_t pid;
  std::string profile;
};
std::vector<Retired> retiring;
int high_fd(int fd) {
  int result = fcntl(fd, F_DUPFD_CLOEXEC, 16);
  close(fd);
  require(result >= 0, "browser_spawn", "Cannot reserve engine descriptor");
  return result;
}
}  // namespace
void reap_public_browsers() {
  for (auto it = retiring.begin(); it != retiring.end();) {
    auto r = waitpid(it->pid, nullptr, WNOHANG);
    if (r == it->pid || (r < 0 && errno == ECHILD)) {
      retire_browser_profile(it->profile);
      it = retiring.erase(it);
    } else
      ++it;
  }
}
PublicBrowser::PublicBrowser(Playback& p, Presentation& ui, Occurrence o,
                             const std::string& launcher, const std::string& profile,
                             const std::string& bundle_root, const std::string& resources,
                             const std::string& backend, bool decoration, bool debugger, const std::string& cache_mode,
                             bool render_ahead, bool allow_direct, Json streams, std::shared_ptr<const CaptureSnapshot> capture)
    : id(o.id),
      backend_(capture && capture->capsule() ? "capsule-chromium" : backend),
      max_submissions_(render_ahead ? 2 : 1),
      playback_(p),
      presentation_(ui),
      occurrence_(std::move(o)),
      profile_(profile),
      offline_(!p.online() || cache_mode == "isolated" ||
               virtual_cache_source(occurrence_.item.at("source"))),
      bundle_root_(bundle_root) {
  initial_display_=occurrence_.display;
  resources_ = resources;
  mixed_cache_ = cache_mode == "mixed";
  capture_replay_ = cache_mode == "isolated";
  restart_on_online_ = !p.online() && cache_mode != "isolated" &&
                       !virtual_cache_source(occurrence_.item.at("source"));
  decoration_ = decoration;
  navigation_url_ = occurrence_.item.at("source").get<std::string>();
  try {
    std::filesystem::create_directories(profile_);
    auto source = occurrence_.item.at("source").get<std::string>();
    capture_ = std::move(capture);
    std::string engine_digest, engine_version;
    if (backend_ == "capsule-chromium") {
      const auto& capsule = capture_->record.at("capsule");
      engine_digest = capsule.value("engineSHA256", "");
      engine_version = capsule.value("engineVersion", "");
      require(valid_engine_digest(engine_digest) && !engine_version.empty(), "capsule_engine",
              "Recast this signed capsule to load its engine pin");
      require(offline_ && capture_replay_, "capsule_engine", "Pinned Chromium requires isolated replay");
      require(std::filesystem::is_regular_file(capsule_engine_root(engine_digest) / "usr/lib/chromium/chromium"),
              "capsule_engine", "Capsule Chromium engine is not installed: " + engine_digest);
    }
    if (!offline_ && !capture_) {
      const auto route = artwork_route(playback_.artwork_sources(), occurrence_.item, source, true);
      routed_document_ = route.url != source;
      navigation_url_ = gateway_document_url(source, route.url);
    }
    if (capture_ && !capture_->file_entry.empty())
      navigation_url_ = capture_->file_entry;
    // Online mixed-mode misses must reach the live origin without passing
    // through a registered capture handler. Large GETs use the live path in
    // that mode; offline and explicitly isolated occurrences use the stream.
    if (offline_) streams_ = std::move(streams);
    if (!streams_.empty()) {
      // The launcher reads this file before entering the fresh sandbox.
      std::ofstream index(profile_ + "/stream-index.json");
      index << streams_;
      index.close();
      require(!index.fail(), "bundle", "Cannot stage capture stream index");
    }
    const auto render_item = capture_ ? capture_->render_item(occurrence_.item) : occurrence_.item;
    auto path = navigation_url_.substr(0, navigation_url_.find_first_of("?#"));
    std::transform(path.begin(), path.end(), path.begin(),
                   [](unsigned char c) { return std::tolower(c); });
    bundled_ = decoration_ || render_item.value("mimeType", "").starts_with("model/") ||
               path.ends_with(".glb") || path.ends_with(".gltf");
    bundle_prefix_ = "https://ff-player.internal/" + uuid() + "/";
    auto bounds = presentation_.bounds(occurrence_.display);
    // Setup decoration and inset artwork need host composition. Fullscreen
    // CEF forwards its Wayland buffers straight to Cage through the relay.
    bool cage=allow_direct && backend_=="cef-views" && !decoration_ && !debugger && !bundled_ &&
              playback_.overlay().is_null() && !playback_.service_failure() &&
              (!playback_.visible() || playback_.state()["settings"].value("transitionMs",0.0)==0) &&
              bounds.width==presentation_.width() && bounds.height==presentation_.height();
    int feedback_child=-1, admission_child=-1;
    if(cage){
      int pair[2];
      require(socketpair(AF_UNIX,SOCK_DGRAM|SOCK_CLOEXEC|SOCK_NONBLOCK,0,pair)==0,"browser_spawn","Cannot create Cage feedback socket");
      feedback_fd_=high_fd(pair[0]);feedback_child=high_fd(pair[1]);
      require(socketpair(AF_UNIX,SOCK_DGRAM|SOCK_CLOEXEC|SOCK_NONBLOCK,0,pair)==0,"browser_spawn","Cannot create window admission socket");
      admission_fd_=high_fd(pair[0]);admission_child=high_fd(pair[1]);
    }else{
    server_ = ff_browser_server_create();
    require(server_, "browser_compositor", "Cannot initialize private Wayland compositor");
    require(ff_browser_server_listen(server_, (profile_ + "/wayland").c_str()) == 0,
            "browser_compositor", "Cannot bind private Wayland socket");
    view_ = ff_browser_view_create(server_, bounds.width, bounds.height, nullptr);
    require(view_, "browser_compositor", "Cannot initialize artwork view");
    }
    int incoming[2], outgoing[2];
    require(pipe2(incoming, O_CLOEXEC | O_NONBLOCK) == 0, "browser_spawn",
            "Cannot create engine command pipe");
    require(pipe2(outgoing, O_CLOEXEC | O_NONBLOCK) == 0, "browser_spawn",
            "Cannot create engine event pipe");
    writer_ = high_fd(incoming[1]);
    reader_ = high_fd(outgoing[0]);
    int child_read = high_fd(incoming[0]), child_write = high_fd(outgoing[1]);
    // Chromium expects blocking child pipe endpoints. Host endpoints stay
    // nonblocking so a wedged engine cannot monopolize the command loop.
    fcntl(child_read, F_SETFL, fcntl(child_read, F_GETFL) & ~O_NONBLOCK);
    fcntl(child_write, F_SETFL, fcntl(child_write, F_GETFL) & ~O_NONBLOCK);
    posix_spawn_file_actions_t actions;
    posix_spawn_file_actions_init(&actions);
    posix_spawn_file_actions_adddup2(&actions, child_read, 3);
    posix_spawn_file_actions_adddup2(&actions, child_write, 4);
    if(cage){
      posix_spawn_file_actions_adddup2(&actions,feedback_child,5);
      posix_spawn_file_actions_adddup2(&actions,admission_child,6);
    }
    posix_spawnattr_t attr;
    posix_spawnattr_init(&attr);
    posix_spawnattr_setflags(&attr, POSIX_SPAWN_SETPGROUP);
    posix_spawnattr_setpgroup(&attr, 0);
    const auto& permissions = p.global_policy();
    auto cache_key = !cache_mode.empty() ? "" : ArtworkCache::browser_key(source, permissions.at("localNetwork") == true);
    auto cache_bytes = std::to_string(permissions.at("cache").at("maxBytes").get<int64_t>());
    auto cache_days = std::to_string(permissions.at("cache").at("maxAgeDays").get<int>());
    char* args[] = {const_cast<char*>(launcher.c_str()), const_cast<char*>("--launch-browser"), const_cast<char*>(profile_.c_str()),
                    const_cast<char*>(offline_ ? "offline" : "online"),
                    const_cast<char*>(debugger ? "debug" : "normal"), const_cast<char*>(cage?"cage":"nested"),
                    const_cast<char*>(permissions.at("localNetwork") == true ? "lan" : "public"),
                    const_cast<char*>(permissions.at("autoplay") == true ? "auto" : "gesture"),
                    cache_key.data(), cache_bytes.data(), cache_days.data(), const_cast<char*>((capture_ ? capture_->root : bundle_root_).c_str()),
                    engine_digest.empty() ? nullptr : engine_digest.data(), nullptr};
    int rc = posix_spawn(&pid_, launcher.c_str(), &actions, &attr, args, environ);
    posix_spawnattr_destroy(&attr);
    posix_spawn_file_actions_destroy(&actions);
    if(feedback_child>=0)close(feedback_child);
    if(admission_child>=0)close(admission_child);
    close(child_read);
    close(child_write);
    require(rc == 0, "browser_spawn", "Cannot launch public Chromium: " + std::to_string(rc));
    send("Browser.setDownloadBehavior", {{"behavior", "deny"}});
    auto set_permission = [&](const char* name, bool allowed) {
      send("Browser.setPermission", {{"permission", {{"name", name}}},
                                     {"setting", allowed ? "granted" : "denied"}});
    };
    for (auto name : {"geolocation","camera","microphone","notifications"})
      set_permission(name, permissions.at(name).get<bool>());
    set_permission("local-network", permissions.at("localNetwork").get<bool>());
    set_permission("loopback-network", false);
    send("Browser.getVersion", Json::object(), "",
         [this, engine_digest, engine_version](Json value) {
           if (!engine_digest.empty()) {
             if (value.value("product", "") != "Chrome/" + engine_version) {
               fail("Installed capsule Chromium version differs from signed version " + engine_version);
               return;
             }
             value["imageSHA256"] = engine_digest;
           }
           playback_.engine_status(id, value);
           send("Target.setAutoAttach",
                {{"autoAttach", true}, {"waitForDebuggerOnStart", true}, {"flatten", true}});
         });
  } catch (...) {
    if (pid_ > 0) {
      kill(-pid_, SIGKILL);
      retiring.push_back({pid_, profile_});
    }
    if (admission_fd_>=0) close(admission_fd_);
  if (feedback_fd_>=0) close(feedback_fd_);
    if (writer_ >= 0) close(writer_);
    if (reader_ >= 0) close(reader_);
    if (server_) ff_browser_server_destroy(server_);
    if (pid_ <= 0) retire_browser_profile(profile_);
    throw;
  }
}
PublicBrowser::~PublicBrowser() {
  cache_->active = false;
  if (pid_ > 0) {
    kill(-pid_, SIGKILL);
    retiring.push_back({pid_, profile_});
  }
  if (admission_fd_>=0) close(admission_fd_);
  if (feedback_fd_>=0) close(feedback_fd_);
    if (writer_ >= 0) close(writer_);
  if (reader_ >= 0) close(reader_);
  presentation_.destroy(frames.capture);
  presentation_.destroy(frames.shown);
  if (server_) ff_browser_server_destroy(server_);
  // Profiles contain only this occurrence's browser state, no application data.
  // Garbage collection belongs off the presentation loop; leave them for the
  // bounded startup sweeper rather than synchronously deleting a large cache.
}
void PublicBrowser::send(const std::string& method, Json params, const std::string& session,
                         std::function<void(const Json&)> reply) {
  require(writes_.size() < 256 && replies_.size() < 256, "browser_backpressure",
          "Public engine command queue exceeded limit");
  if (params.is_null()) params = Json::object();
  Json request = {{"id", ++sequence_}, {"method", method}, {"params", params}};
  if (!session.empty()) request["sessionId"] = session;
  auto data = request.dump();
  require(data.size() < 48 * 1024 * 1024, "browser_limit", "Engine command exceeds limit");
  require(queued_bytes_ + data.size() < 64 * 1024 * 1024, "browser_backpressure",
          "Engine output exceeds 64 MiB");
  queued_bytes_ += data.size() + 1;
  data.push_back('\0');
  writes_.push_back({std::move(data), 0, method == "Page.getLayoutMetrics"});
  replies_.emplace(sequence_,
                   Reply{method, session, reply ? std::move(reply) : [](const Json&) {}});
}
void PublicBrowser::fail(const std::string& error, bool retry_composited) {
  if (failed_) return;
  failed_ = true;
  const bool retry_selected=retry_composited && direct() && playback_.desired() && playback_.desired()->id==id;
  if (!decoration_ && !retry_selected) playback_.failed(id, error);
  if (pid_ > 0) kill(-pid_, SIGKILL);
}
void PublicBrowser::check_sketch_startup() {
  if (!check_sketch_startup_) return;
  check_sketch_startup_ = false;
  send("Runtime.evaluate", {{"expression", sketch_startup_expression}, {"returnByValue", true},
                            {"throwOnSideEffect", true}, {"silent", true}, {"timeout", 150}},
       session_, [this, generation = document_generation_](const Json& reply) {
         if (generation != document_generation_ || reply.contains("exceptionDetails")) return;
         const auto state = sketch_startup_state(reply.value("result", Json::object()).value("value", Json()));
         // Global-mode p5 can wait for translations after document load.
         // Instance-mode artwork can legitimately keep the global instance null.
         check_sketch_startup_ = state == SketchStartup::pending ||
             (state == SketchStartup::awaiting_instance &&
              std::chrono::steady_clock::now() - loaded_at_ < std::chrono::seconds(30));
         if (state == SketchStartup::failed) {
           std::fprintf(stderr, "Artwork %s: p5 setup incomplete with negative preload count and zero draws\n",
                        id.c_str());
           throw Error("sketch_startup", "Artwork could not finish sketch setup before its first frame");
         }
       });
}
void PublicBrowser::continue_live(const std::string& request, const std::string& session, const Json& network) {
  auto url = network.at("url").get<std::string>();
  auto route = artwork_route(playback_.artwork_sources(), occurrence_.item, url, true);
  Json params = {{"requestId", request}};
  if (route.url != url) params["url"] = route.url;
  if (!route.user_agent.empty()) {
    Json headers = Json::array();
    for (const auto& [name,value] : network.at("headers").items())
      if (strcasecmp(name.c_str(), "User-Agent")) headers.push_back({{"name",name},{"value",value}});
    headers.push_back({{"name","User-Agent"},{"value",route.user_agent}});
    params["headers"] = headers;
  }
  send("Fetch.continueRequest", params, session);
}
bool PublicBrowser::continue_mixed_request(const std::string& session, const std::string& request) {
  auto pending = mixed_requests_.extract(session + "\n" + request);
  if (pending.empty()) return false;
  ++cache_misses_;
  continue_live(request, session, pending.mapped());
  return true;
}
void PublicBrowser::message(const Json& m) {
  if (m.contains("id")) {
    auto it = replies_.find(m["id"].get<uint64_t>());
    if (it != replies_.end()) {
      auto reply = std::move(it->second);
      replies_.erase(it);
      if (m.contains("error")) {
        auto text = m.at("error").value("message", "");
        // This optional read may race navigation or a disabled scripting
        // context. Only an affirmative bootstrap result can fail an artwork.
        if (reply.method == "Runtime.evaluate") {
          reply.callback(Json{{"exceptionDetails", m.at("error")}});
          return;
        }
        // A short-lived worker/popup can close while its typed setup is queued.
        // Its stale session cannot invalidate the main artwork document.
        if (reply.session != session_ &&
            (text == "Inspected target navigated or closed" || text.starts_with("Session closed")))
          return;
        if ((reply.method == "Fetch.continueRequest" || reply.method == "Fetch.fulfillRequest" ||
             reply.method == "Fetch.failRequest") &&
            text.find("Invalid InterceptionId") != std::string::npos)
          return;
        throw Error("browser_protocol", reply.method + ": " + m.at("error").dump());
      }
      reply.callback(m.value("result", Json::object()));
    }
    return;
  }
  auto method = m.value("method", "");
  const auto p = m.value("params", Json::object());
  auto session = m.value("sessionId", "");
  auto enable_interception = [&](const std::string& child) {
    Json patterns = Json::array();
    if (bundled_ || mixed_cache_ || capture_replay_) {
      patterns.push_back({{"urlPattern", "*"}});
    } else {
      auto hosts = playback_.artwork_sources().value("gatewayHosts", Json::array());
      if (occurrence_.item.contains("ffObservedGateway")) {
        auto source = occurrence_.item.at("source").get<std::string>();
        auto uri = g_uri_parse(source.c_str(), G_URI_FLAGS_ENCODED, nullptr);
        if (uri && g_uri_get_host(uri)) hosts.push_back(g_uri_get_host(uri));
        if (uri) g_uri_unref(uri);
      }
      for (const auto& host : hosts) {
        auto authority = host.get<std::string>();
        patterns.push_back({{"urlPattern", "*://" + authority + "/*"}});
        patterns.push_back({{"urlPattern", "*://" + authority + ":*/*"}});
      }
    }
    if (!patterns.empty()) send("Fetch.enable", {{"patterns", patterns}}, child);
  };
  if (method == "Page.javascriptDialogOpening") {
    send("Page.handleJavaScriptDialog", {{"accept", false}}, session);
  } else if (method == "Target.attachedToTarget") {
    auto child = p.at("sessionId").get<std::string>();
    auto type = p.at("targetInfo").value("type", "");
    if (type == "page" && session_.empty()) {
      session_ = child;
      send("Page.enable", {}, child);
      send("Network.enable", {}, child);
      if (routed_document_) {
        send("Audits.enable", {}, child);
        send("Runtime.enable", {}, child);
      }
      send("Page.setLifecycleEventsEnabled", {{"enabled", true}}, child);
      enable_interception(child);
      send("Target.setAutoAttach",
           {{"autoAttach", true}, {"waitForDebuggerOnStart", true}, {"flatten", true}}, child);
      send("Runtime.runIfWaitingForDebugger", {}, child);
      // Request events can precede the navigate reply. Identify the root
      // frame before starting the load so child documents cannot claim it.
      send("Page.getFrameTree", {}, child, [this, child](const Json& tree) {
        main_frame_ = tree.at("frameTree").at("frame").at("id").get<std::string>();
        send("Page.navigate",
             {{"url", bundled_ ? Json(bundle_prefix_ +
                                     (decoration_ ? "setup-artwork/index.html" : "index.html"))
                              : Json(navigation_url_)}},
             child, [this](const Json& result) {
               navigation_error_ = result.value("errorText", "");
             });
      });
    } else if (type == "page") {
      // Popups never become device windows, including script-created blank tabs.
      send("Target.closeTarget", {{"targetId", p.at("targetInfo").at("targetId")}});
    } else {
      if (offline_ && (type == "service_worker")) {
        ++unsupported_worker_targets_;
        cache_coverage_ = false;
        playback_.runtime_notice(
            id,
            "Service-worker execution is not available through this offline replay adapter. "
            "Dedicated-worker resource replay is tested; service-worker dependencies remain "
            "unverified.");
      }
      if (type == "iframe")
        enable_interception(child);
      if (type == "iframe")
        send("Target.setAutoAttach",
             {{"autoAttach", true}, {"waitForDebuggerOnStart", true}, {"flatten", true}}, child);
      send("Runtime.runIfWaitingForDebugger", {}, child);
    }
  } else if (routed_document_ && session == session_ && method == "Runtime.executionContextCreated") {
    const auto& context = p.at("context");
    const auto aux = context.value("auxData", Json::object());
    if (aux.value("isDefault", false) && aux.value("frameId", "") == main_frame_)
      main_world_ = context.at("id");
  } else if (routed_document_ && session == session_ && method == "Runtime.exceptionThrown") {
    const auto& details = p.at("exceptionDetails");
    if (main_world_ && details.value("executionContextId", 0) == main_world_ &&
        details.value("exception", Json::object()).value("className", "") == "ReferenceError") {
      const auto description = details.at("exception").value("description", "");
      if (description.starts_with("ReferenceError: fxhash is not defined") ||
          description.starts_with("ReferenceError: fxrand is not defined"))
        missing_init_binding_ = true;
    }
  } else if (routed_document_ && session == session_ && method == "Audits.issueAdded") {
    const auto& issue = p.at("issue");
    if (issue.value("code", "") == "ContentSecurityPolicyIssue") {
      const auto policy = issue.at("details").value("contentSecurityPolicyIssueDetails", Json::object());
      if (!policy.value("isReportOnly", true) &&
          policy.value("contentSecurityPolicyViolationType", "") == "kInlineViolation" &&
          policy.value("violatedDirective", "").starts_with("script-src") &&
          policy.value("sourceCodeLocation", Json::object()).value("url", "") == navigation_url_)
        blocked_init_script_ = true;
    }
  } else if (method == "Page.frameNavigated" && session == session_) {
    const auto& frame = p.at("frame");
    if (!frame.contains("parentId")) {
      ++document_generation_;
      checking_bootstrap_ = false;
      loaded_ = false;
      started_ = std::chrono::steady_clock::now();
      check_sketch_startup_ = false;
      main_world_ = 0;
      missing_init_binding_ = blocked_init_script_ = false;
      main_frame_ = frame.at("id").get<std::string>();
      document_url_ = frame.at("url").get<std::string>() + frame.value("urlFragment", "");
    }
  } else if (method == "Page.navigatedWithinDocument" && session == session_ &&
             p.value("frameId", "") == main_frame_) {
    document_url_ = p.at("url").get<std::string>();
  } else if (method == "Fetch.requestPaused") {
    auto request = p.at("requestId").get<std::string>();
    auto network = p.at("request");
    auto url = network.value("url", "");
    if (bundled_request(p, session)) return;
    if (!url.starts_with("http://") && !url.starts_with("https://")) {
      send("Fetch.continueRequest", {{"requestId", request}}, session);
      return;
    }
    const bool local_chain = virtual_cache_source(url);
    const bool replay = capture_replay_ || local_chain || mixed_cache_;
    if (!replay) {
      continue_live(request, session, network);
      return;
    }
    Json query = {{"source", occurrence_.item.at("source")},
                  {"url", url},
                  {"method", network.value("method", "GET")},
                  {"headers", network.value("headers", Json::object())}};
    if (mixed_cache_ && !offline_ && !local_chain)
      mixed_requests_[session + "\n" + request] = query;
    if (cache_queue_.size() + cache_reading_ >= 64) {
      if (continue_mixed_request(session, request)) return;
      send("Fetch.failRequest", {{"requestId", request}, {"errorReason", "InsufficientResources"}}, session);
    } else {
      cache_queue_.push_back({session, request, std::move(query)});
    }
  } else if (method == "Network.requestWillBeSent" && session == session_ &&
             p.value("type", "") == "Document" && p.value("frameId", "") == main_frame_) {
    main_document_request_ = p.at("requestId").get<std::string>();
    if (p.contains("redirectResponse") && p.at("redirectResponse").value("url", "") == navigation_url_)
      navigation_url_ = p.at("request").value("url", navigation_url_);
  } else if (method == "Network.responseReceived" && session == session_ &&
             p.value("frameId", "") == main_frame_) {
    const auto& response = p.at("response");
    const auto type = p.value("type", "");
    const bool main_document = type == "Document" && !main_document_request_.empty() &&
                               p.value("requestId", "") == main_document_request_;
    const bool initial_source = !initial_response_seen_ &&
                                response.value("url", "") == navigation_url_ &&
                                (main_document || type == "Media" || type == "Image");
    if (!main_document && !initial_source) return;
    // A script or meta refresh can navigate after the first successful load.
    // Validate each top-level response, but classify the original source once.
    const auto status = response.value("status", 0);
    if (status >= 400) {
      fail("Artwork HTTP " + std::to_string(status));
      return;
    }
    if (!initial_source) return;
    initial_response_seen_ = true;
    auto mime = response.value("mimeType", "");
    Json classified = capture_ ? capture_->render_item(occurrence_.item) : occurrence_.item;
    classified["mimeType"] = mime;
    if (backend_ != "capsule-chromium" && native_media(classified)) resolved_native_mime = mime;
  } else if (method == "Page.lifecycleEvent" && (!bundled_ || decoration_) && session == session_ &&
             p.value("name", "") == "load" && p.value("frameId", "") == main_frame_) {
    if(document_url_.empty() || document_url_=="about:blank")return;
    loaded_ = true;
    if (routed_document_) {
      checking_bootstrap_ = true;
      send("Runtime.evaluate", {{"expression",
           "/\\bvar\\s+fxrand\\b/.test(document.querySelector('script#fxhash-snippet:not([src])')?.textContent || '') && "
           "typeof fxhash === 'undefined' && typeof fxrand === 'undefined'"},
           {"returnByValue", true}, {"throwOnSideEffect", true}, {"silent", true}, {"timeout", 250}},
           session_, [this, generation = document_generation_](const Json& result) {
             if (generation != document_generation_) return;
             checking_bootstrap_ = false;
             if (result.value("result", Json::object()).value("value", false) ||
                 (result.contains("exceptionDetails") && blocked_init_script_ && missing_init_binding_))
               fail(blocked_init_script_ ? "Artwork bootstrap blocked by gateway Content-Security-Policy"
                                         : "Artwork edition bootstrap did not initialize", false);
           });
    }
    loaded_at_=std::chrono::steady_clock::now();
    navigation_load_ns_=static_cast<uint64_t>(p.value("timestamp",0.)*1e9);
    load_frame_ = direct()?direct_frames_:frames.count;
    if(view_)ff_browser_view_redraw(view_);
    if (!decoration_) playback_.committed(id);
    if (!bundled_) {
      check_sketch_startup_ = true;
      check_sketch_startup();
    }
  } else if (method == "Network.loadingFailed" && session == session_ &&
             p.value("type", "") == "Document" && !main_document_request_.empty() &&
             p.value("requestId", "") == main_document_request_ && !p.value("canceled", false))
    fail("Artwork navigation failed: " + p.value("errorText", "network error"));
  else if (method == "Target.targetCrashed" || method == "Inspector.targetCrashed")
    fail("Public Chromium renderer crashed");

}
bool PublicBrowser::bundled_request(const Json& p, const std::string& session) {
  if (!bundled_) return false;
  auto url = p.at("request").value("url", "");
  if (!url.starts_with(bundle_prefix_)) return false;
  auto request = p.at("requestId");
  auto path = url.substr(bundle_prefix_.size());
  std::string data, mime = "application/octet-stream";
  if (session != session_) {
    send("Fetch.failRequest", {{"requestId", request}, {"errorReason", "AccessDenied"}}, session);
    return true;
  }
  if (path == "config.json") {
    const auto render_item = capture_ ? capture_->render_item(occurrence_.item) : occurrence_.item;
    Json config = {{"source", render_item.at("source")},
                   {"mime", render_item.value("mimeType", "")},
                   {"display", occurrence_.display},
                   {"displayRevision", occurrence_.display_revision},
                   {"paused", playback_.media_paused(id)}};
    config["display"]["background"] =
        display_color(occurrence_.display.value("background", "#000000"));
    data = config.dump();
    mime = "application/json";
  } else if (path.starts_with("events/")) {
    auto event = path.substr(7);
    if (event == "ready") {
      loaded_ = true;
      load_frame_ = direct()?direct_frames_:frames.count;
      if(view_)ff_browser_view_redraw(view_);
      if (!decoration_) playback_.committed(id);
    } else if (event.starts_with("configured/")) {
      auto revision = std::to_string(occurrence_.display_revision);
      if (event.substr(11) == revision && configured_revision_ != occurrence_.display_revision) {
        configured_revision_ = occurrence_.display_revision;
        configuration_frame_ = frames.count;
      }
    } else if (event == "error")
      fail("Bundled GLB renderer reported a loading or graphics error");
    data = "{}";
    mime = "application/json";
  } else {
    require(
        path.find("..") == std::string::npos && path.find_first_of("?%#\\") == std::string::npos,
        "bundle_resource", "Invalid bundled resource path");
    auto root = std::filesystem::canonical(resources_);
    auto file = std::filesystem::canonical(root / path);
    require(file.string().starts_with(root.string() + "/"), "bundle_resource",
            "Bundled resource outside root");
    require(std::filesystem::file_size(file) <= 32 * 1024 * 1024, "bundle_resource",
            "Bundled resource exceeds limit");
    std::ifstream input(file, std::ios::binary);
    data.assign(std::istreambuf_iterator<char>(input), {});
    require(bool(input) || input.eof(), "bundle_resource", "Cannot read bundled resource");
    if (path.ends_with(".js"))
      mime = "text/javascript";
    else if (path.ends_with(".html"))
      mime = "text/html";
    else if (path.ends_with(".wasm"))
      mime = "application/wasm";
  }
  send("Fetch.fulfillRequest",
       {{"requestId", request},
        {"responseCode", 200},
        {"responseHeaders", Json::array({{{"name", "Content-Type"}, {"value", mime}},
                                         {{"name", "Cache-Control"}, {"value", "no-store"}}})},
        {"body", base64(data)}},
       session);
  return true;
}
void PublicBrowser::poll() {
  if (failed_) return;
  try {
    for (int count = 0; count < 64 && !writes_.empty(); ++count) {
      auto& pending = writes_.front();
      auto n = write(writer_, pending.data.data() + pending.offset,
                     pending.data.size() - pending.offset);
      if (n < 0 && (errno == EAGAIN || errno == EINTR)) break;
      require(n > 0, "browser_exit", "Engine command pipe closed");
      pending.offset += n;
      if (pending.offset == pending.data.size()) {
        if (pending.health_probe) health_.written(std::chrono::steady_clock::now());
        queued_bytes_ -= pending.data.size();
        writes_.pop_front();
      }
    }
    char block[65536];
    for (int count = 0; count < 16; ++count) {
      auto n = read(reader_, block, sizeof(block));
      if (n < 0 && (errno == EAGAIN || errno == EINTR)) break;
      require(n > 0, "browser_exit", "Public Chromium event pipe closed");
      buffer_.append(block, n);
      require(buffer_.size() < 48 * 1024 * 1024, "browser_limit", "Engine message exceeds 48 MiB");
    }
    for (int count = 0; count < 128; ++count) {
      auto end = buffer_.find('\0');
      if (end == std::string::npos) break;
      auto value = Json::parse(buffer_.substr(0, end));
      buffer_.erase(0, end + 1);
      message(value);
    }
    const auto probe = health_.poll(std::chrono::steady_clock::now(), loaded_);
    require(probe != CefHealthProbe::Action::write_timeout, "browser_backpressure",
            "Could not write the artwork layout health probe while polling the DevTools pipe");
    require(probe != CefHealthProbe::Action::reply_timeout, "renderer_unresponsive",
            "Artwork renderer did not answer its layout health probe during 5 seconds of polling");
    if (probe == CefHealthProbe::Action::send)
      send("Page.getLayoutMetrics", Json::object(), session_, [this](const Json&) {
        health_.answered();
        check_sketch_startup();
      });
    // Only materialize one archived response at a time. A 32 MiB range
    // expands to almost 43 MiB on the pipe; drain it before admitting the
    // next response so parallel fetches cannot exhaust the output budget.
    if (!cache_reading_ && !cache_queue_.empty() && queued_bytes_ < 1024 * 1024) {
      auto& next = cache_queue_.front();
      if (cache_request(cache_, bundle_root_, next.session, next.request, next.query, false, false, capture_)) {
        cache_reading_ = true;
      } else if (!continue_mixed_request(next.session, next.request)) {
        send("Fetch.failRequest", {{"requestId", next.request}, {"errorReason", "InsufficientResources"}}, next.session);
      }
      cache_queue_.pop_front();
    }
    auto cached = queued_bytes_ < 1024 * 1024 ? cache_->take() : std::deque<CacheReply>{};
    for (auto& reply : cached) {
      cache_reading_ = false;
      const auto mixed_key = reply.session + "\n" + reply.request;
      if (reply.error.empty() && reply.metadata.contains("nativeMime") &&
          reply.session == session_ && !bundled_) {
        Json classified = capture_ ? capture_->render_item(occurrence_.item) : occurrence_.item;
        classified["mimeType"] = reply.metadata.at("nativeMime");
        require(native_media(classified), "cache_protocol", "Invalid native media handoff");
        resolved_native_mime = reply.metadata.at("nativeMime");
        // The host replaces this browser with a network-isolated native worker.
        // Legacy response bytes/headers never execute in an artist document.
        continue;
      }
      if (reply.metadata.contains("streamHash")) {
        const auto url = reply.metadata.at("url").get<std::string>();
        if (streams_.contains(url) && streams_.at(url).at("hash") == reply.metadata.at("streamHash")) {
          mixed_requests_.erase(mixed_key);
          ++cache_hits_;
          send("Fetch.continueRequest", {{"requestId", reply.request}}, reply.session);
          continue;
        }
        reply.error = "Capture changed while the browser was running";
      }
      if (!reply.error.empty() && continue_mixed_request(reply.session, reply.request)) continue;
      mixed_requests_.erase(mixed_key);
      if (!reply.error.empty()) {
        ++cache_misses_;
        playback_.runtime_notice(id,
                                 "A captured resource or live service was unavailable. Playback "
                                 "may be incomplete; runtime status records the cache misses.");
        send("Fetch.failRequest",
             {{"requestId", reply.request}, {"errorReason", "InternetDisconnected"}},
             reply.session);
      } else {
        ++cache_hits_;
        cache_coverage_ = reply.metadata.value("complete", false);
        // Locally replayed bytes already have an owner. Only live HTTP
        // responses should populate the browser's disposable artwork cache.
        auto headers = reply.metadata.at("headers");
        headers.push_back({{"name", "Cache-Control"}, {"value", "no-store"}});
        send("Fetch.fulfillRequest",
             {{"requestId", reply.request},
              {"responseCode", reply.metadata.at("status")},
              {"responseHeaders", headers},
              {"body", base64(reply.body)}},
             reply.session);
      }
    }
    if(direct()){
      if(!failed_ && loaded_ && !checking_bootstrap_ && !window_admitted_ && playback_.desired() && playback_.desired()->id==id &&
         playback_.overlay().is_null() && !playback_.service_failure() && !playback_.thermal_hold()){
        const char show=1;
        window_admitted_=::send(admission_fd_,&show,1,MSG_DONTWAIT|MSG_NOSIGNAL)==1;
      }
      int32_t packet[3]{};
      for(int i=0;i<256;++i){
        auto bytes=recv(feedback_fd_,packet,sizeof(packet),MSG_DONTWAIT|MSG_TRUNC);
        if(bytes<=0)break;
        if(bytes==sizeof(uint64_t)){
          uint64_t timestamp;std::memcpy(&timestamp,packet,sizeof(timestamp));
          last_presentation_ns_=std::max(last_presentation_ns_,timestamp);
          ++direct_frames_;
        }else if(bytes==sizeof(packet)){
          physical_pointer=PointerSample{packet[1],packet[2],packet[0]!=0 || (physical_pointer && physical_pointer->clicked)};
        }
      }
    }
    playback_.media_status(
        id, {{"backend", backend_},
             {"browserPid", pid_},
             {"documentUrl", document_url_},
             {"inlineScriptBlocked", blocked_init_script_}, {"startupReferenceError", missing_init_binding_},
             {"nestedCompositorGpuPasses", compositor_gpu_passes_},
             {"exportedFrames", frames.count},{"presentedFrames",direct_frames_},{"presentationPath",direct()?"cage":"composited"},
             {"offline", offline_},
             {"cacheHits", cache_hits_},
             {"cacheMisses", cache_misses_},
             {"captureCoverageComplete", cache_coverage_ && unsupported_worker_targets_ == 0},
             {"unsupportedWorkerTargets", unsupported_worker_targets_}});
    require(navigation_error_.empty() || !resolved_native_mime.empty(), "browser_navigation",
            navigation_error_);
    if(!direct()){
    while (!submissions_.empty()) {
      const auto& frame = submissions_.front();
      const auto sample = presentation_.outcome(frame.host_serial);
      const auto elapsed = std::chrono::steady_clock::now() - frame.submitted;
      const bool confirmed = sample.serial && (!sample.timestamp_ns || sample.clock == CLOCK_MONOTONIC);
      if (!confirmed && elapsed < std::chrono::milliseconds(500)) break;
      // Only a matching presentation gets a timestamp. Explicit discard and
      // timeout both release this commit without reporting fictitious scanout.
      ff_browser_view_complete(view_, frame.nested_commit, confirmed ? sample.timestamp_ns : 0,
                               sample.refresh_ns, sample.flags);
      presentation_.browser_feedback(std::chrono::duration<double, std::milli>(elapsed).count(),
                                     !confirmed);
      submissions_.pop_front();
    }
    ff_browser_server_poll(server_);
    // The blit and the final draw use the same GL context. Their GPU command
    // order is sufficient for display. Only producer reuse needs a CPU-side
    // fence check. The fence moves with its texture when FramePair swaps.
    const bool source_released = presentation_.copy_ready(frames.shown) &&
                                 presentation_.copy_ready(frames.capture);
    // After a swap the old shown buffer becomes the spare. Release its lock as
    // soon as the last GPU read completes, even while the producer is idle.
    const bool writable = presentation_.import_read_complete(frames.capture);
    if (!frames.pending && writable && frames.capture.producer_buffer)
      presentation_.destroy(frames.capture);
    if (writable && source_released && submissions_.size() < max_submissions_ &&
        (!frames.pending || !loaded_ || frames.count <= load_frame_ ||
         frames.capture.display_revision != occurrence_.display_revision)) {
      ff_browser_frame frame{};
      auto result = ff_browser_view_frame(view_, &frame);
      require(result >= 0, "browser_acceleration",
              "Public engine requires accelerated DMA-BUF surfaces");
      if (result > 0) {
        require(frame.count > 0 && frame.count <= 4, "browser_acceleration",
                "Invalid browser DMA plane count");
        compositor_gpu_passes_ += frame.compositor_copies;
        DmaFrame dma{};
        dma.width = frame.width;
        dma.height = frame.height;
        dma.count = frame.count;
        dma.format = frame.format;
        dma.modifier = frame.modifier;
        for (int i = 0; i < frame.count; ++i)
          dma.planes[i] = {frame.fd[i], frame.stride[i], frame.offset[i]};
        // An explicit wlroots lock keeps the DMA-BUF valid through later UI
        // draws. Non-RGB exports use the existing fenced blit fallback.
        if (frames.pending) presentation_.coalesced_frame();
        frames.pending = false;
        std::shared_ptr<void> producer(ff_browser_view_lock_frame(view_), ff_browser_frame_unlock);
        // Chromium 158 drops embedded 4K H.264 frames when its buffers stay
        // locked through display. The fenced blit releases them sooner. CEF
        // Views 156 sustained the same clip with direct RGB sampling on FF1.
        presentation_.accelerated(frames.capture, dma, std::move(producer), backend_ == "cef-views");
        ff_browser_view_frame_requested(view_);
        auto b = presentation_.bounds(occurrence_.display);
        frames.capture.display_revision =
            frame.layout_ready && frame.width == b.width && frame.height == b.height &&
                    (!bundled_ || decoration_ ||
                     (configured_revision_ == occurrence_.display_revision &&
                      frames.count + 1 > configuration_frame_))
                ? occurrence_.display_revision
                : 0;
        frames.captured();
        if (!loaded_ || frames.count <= load_frame_ || !frames.capture.display_revision) ff_browser_view_presented(view_);
      }
    }
    }
    int status;
    auto exit = waitpid(pid_, &status, WNOHANG);
    if (exit == pid_) {
      pid_ = -1;
      retire_browser_profile(profile_);
      throw Error("browser_exit", "Public Chromium exited: " + std::to_string(status));
    }
    require(loaded_ || std::chrono::steady_clock::now() - started_ < std::chrono::seconds(30),
            "browser_timeout", "Artwork did not complete navigation within 30 seconds");
    update();
  } catch (const std::exception& error) {
    fail(error.what());
  }
}
bool PublicBrowser::has_ready_frame() const {
  if (checking_bootstrap_) return false;
  if(direct())return !failed_ && resolved_native_mime.empty() && loaded_ && navigation_load_ns_ && last_presentation_ns_>=navigation_load_ns_;
  return !failed_ && resolved_native_mime.empty() && loaded_ && frames.count > load_frame_ &&
         frames.latest().display_revision == occurrence_.display_revision;
}
void PublicBrowser::did_present() {
  if (!frames.pending) return;
  frames.submitted();
  if (decoration_ || !presentation_.frame_serial()) {
    ff_browser_view_presented(view_);
    return;
  }
  if (auto commit = ff_browser_view_take_commit(view_))
    submissions_.push_back({presentation_.swap_serial(), commit, std::chrono::steady_clock::now()});
}
void PublicBrowser::update() {
  const auto previous_revision=occurrence_.display_revision;
  const auto old_bounds=presentation_.bounds(occurrence_.display);
  if (playback_.desired() && playback_.desired()->id == id) {
    occurrence_.display = playback_.desired()->display;
    occurrence_.display_revision = playback_.desired()->display_revision;
  } else if (playback_.visible() && playback_.visible()->id == id) {
    occurrence_.display = playback_.visible()->display;
    occurrence_.display_revision = playback_.visible()->display_revision;
  }
  auto b = presentation_.bounds(occurrence_.display);
  if(view_){
    // Some CEF Views builds retain the old fullscreen buffer indefinitely.
    // If no reflow reaches composition, reopen at the requested dimensions.
    // A fresh worker begins with those dimensions and does not arm this timer.
    if(b.width!=old_bounds.width || b.height!=old_bounds.height)
      resize_started_at_=std::chrono::steady_clock::now();
    const auto& composed=playback_.composition();
    if(resize_started_at_ && composed.is_object() && composed.value("occurrence","")==id &&
       composed.value("displayRevision",uint64_t(0))==occurrence_.display_revision)
      resize_started_at_.reset();
    ff_browser_view_resize(view_, b.width, b.height);
    if(previous_revision!=occurrence_.display_revision &&
       b.width==old_bounds.width && b.height==old_bounds.height)ff_browser_view_redraw(view_);
  }
}
void PublicBrowser::input(const Json& p) {
  if (failed_) return;
  auto kind = p.value("kind", "");
  if(direct()){
    if(session_.empty())return;
    if(kind=="move" || kind=="button" || kind=="wheel"){
      mouse_x_=p.value("x",mouse_x_);mouse_y_=p.value("y",mouse_y_);
      const unsigned held=p.value("buttons",0u);
      const unsigned buttons=(held&1u)|((held&2u)<<1)|((held&4u)>>1);
      const unsigned modifiers=(p.value("alt",false)?1:0)|(p.value("control",false)?2:0)|
                               (p.value("meta",false)?4:0)|(p.value("shift",false)?8:0);
      Json event={{"x",mouse_x_},{"y",mouse_y_},{"buttons",buttons},{"modifiers",modifiers}};
      if(kind=="move")event["type"]="mouseMoved";
      if(kind=="wheel"){event["type"]="mouseWheel";event["deltaX"]=-p.value("dx",0.);event["deltaY"]=-p.value("dy",0.);}
      if(kind=="button"){
        event["type"]=p.value("down",false)?"mousePressed":"mouseReleased";
        auto b=p.value("button",1);event["button"]=b==1?"left":b==2?"middle":"right";
        event["clickCount"]=p.value("clicks",1);
      }
      send("Input.dispatchMouseEvent",event,session_);return;
    }
    if(kind=="blur")return;
  }
  if (kind == "blur") {
    ff_browser_view_blur(view_);
    return;
  }
  if(view_)ff_browser_view_modifiers(view_, p.value("shift", false), p.value("control", false),
                            p.value("alt", false), p.value("meta", false));
  if (kind == "move" || kind == "button")
    ff_browser_view_pointer(view_, p.value("x", 0), p.value("y", 0));
  if (kind == "button") {
    auto b = p.value("button", 1);
    ff_browser_view_button(view_,
                           b == 1   ? BTN_LEFT
                           : b == 2 ? BTN_MIDDLE
                                    : BTN_RIGHT,
                           p.value("down", false));
  } else if (kind == "wheel")
    // Chromium converts Wayland axis units to 12 CSS pixels. The protocol
    // retains CSS-pixel deltas, matching the previous phone input adapter.
    ff_browser_view_wheel(view_, -p.value("dx", 0) / 12.0, -p.value("dy", 0) / 12.0);
  else if (kind == "key" && !session_.empty()) {
    // Wayland key presses synthesize printable text. The player protocol keeps
    // raw physical keys separate, matching the phone's raw+text sequence.
    if(view_)ff_browser_view_focus(view_);
    send("Input.dispatchKeyEvent", browser_key(p), session_);
  } else if (kind == "text" && !session_.empty()) {
    if(view_)ff_browser_view_focus(view_);
    auto text = p.at("text").get<std::string>();
    if (text == "\n" || text == "\r")
      send("Input.dispatchKeyEvent",
           {{"type", "char"},
            {"key", "Enter"},
            {"windowsVirtualKeyCode", 13},
            {"text", "\r"},
            {"unmodifiedText", "\r"}},
           session_);
    else if (text.size() == 1 && uint8_t(text[0]) >= 32 && uint8_t(text[0]) < 127)
      send("Input.dispatchKeyEvent", {{"type", "char"}, {"text", text}, {"unmodifiedText", text}},
           session_);
    else
      send("Input.insertText", {{"text", text}}, session_);
  }
}
}  // namespace ff
