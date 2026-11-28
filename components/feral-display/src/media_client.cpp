#include "ff/media_client.hpp"

#include <drm_fourcc.h>
#include <fcntl.h>
#include <spawn.h>
#include <linux/openat2.h>
#include <sys/syscall.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <csignal>
#include <iostream>

#include "ff/inline_media.hpp"
#include "ff/media_ipc.hpp"
extern char** environ;

namespace ff {
namespace {
std::vector<pid_t> retiring;
}
void reap_media_workers() {
  for (auto it = retiring.begin(); it != retiring.end();) {
    auto result = waitpid(*it, nullptr, WNOHANG);
    if (result == *it || (result < 0 && errno == ECHILD))
      it = retiring.erase(it);
    else
      ++it;
  }
}
MediaClient::MediaClient(Playback& playback, Presentation& presentation, Occurrence occurrence,
                         const std::string& worker, const std::string& bundle_root,
                         const std::string& cache_mode, ArtworkCache* artwork_cache,
                         std::shared_ptr<const CaptureSnapshot> capture)
    : id(occurrence.id),
      artwork_cache_(artwork_cache),
      playback_(playback),
      presentation_(presentation),
      occurrence_(std::move(occurrence)),
      worker_(worker) {
  bundle_root_ = bundle_root;
  mixed_cache_ = cache_mode == "mixed";
  auto engine = playback_.snapshot().value("engine", Json::object());
  if (engine.is_object()) user_agent_ = engine.value("userAgent", "");
  const auto source = occurrence_.item.at("source").get<std::string>();
  const auto name = local_artwork_name(source);
  if (!name.empty() || source.starts_with("data:")) {
    int fd;
    if (name.empty()) {
      fd = inline_media_fd(source);
    } else {
      int root = open(local_artworks, O_DIRECTORY | O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
      require(root >= 0, "local_media", "Cannot open local artwork directory");
      open_how how{};
      how.flags = O_RDONLY | O_NONBLOCK | O_CLOEXEC;
      how.resolve = RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS;
      fd = syscall(SYS_openat2, root, name.c_str(), &how, sizeof(how));
      close(root);
      struct stat info{};
      if (fd < 0 || fstat(fd, &info) || !S_ISREG(info.st_mode)) {
        if (fd >= 0) close(fd);
        throw Error("local_media", "Expected a regular file under the local artwork directory");
      }
    }
    try {
      start(fd);
    } catch (...) {
      close(fd);
      throw;
    }
    close(fd);
  } else if (!bundle_root.empty() &&
             (!cache_mode.empty() ||
              virtual_cache_source(occurrence_.item.at("source"))) &&
             !occurrence_.item.at("source").get<std::string>().starts_with("data:")) {
    auto entry = source;
    if (capture) {
      if (!capture->file_entry.empty()) entry = source_override_ = capture->file_entry;
      occurrence_.item = capture->render_item(occurrence_.item);
    }
    cache_ = std::make_shared<CacheInbox>();
    require(cache_request(cache_, bundle_root, "", id,
                          {{"source", source},
                           {"url", entry},
                           {"method", "GET"},
                           {"headers", Json::object()}},
                          true, false, std::move(capture)),
            "cache_backpressure", "Cannot enqueue cached media open");
  } else
    start_live();
}
void MediaClient::start_live() {
  int fd = -1;
  auto route = artwork_route(playback_.artwork_sources(), occurrence_.item, occurrence_.item.at("source"));
  source_override_ = route.url;
  if (!route.user_agent.empty()) user_agent_ = route.user_agent;
  const auto& url = route.url;
  if (artwork_cache_) {
    warm_request_ = ArtworkCache::media_request(url, user_agent_, playback_.global_policy().at("localNetwork") == true);
    if (warm_request_) {
      fd = ArtworkCache::open_media(warm_request_->key, playback_.global_policy().at("cache").at("maxAgeDays"));
      if (fd >= 0) {
        local_media_key_ = warm_request_->key;
        warm_request_.reset();
      }
    }
  }
  try {
    start(fd);
  } catch (...) {
    if (fd >= 0) close(fd);
    throw;
  }
  if (fd >= 0) close(fd);
}
void MediaClient::start(int cached_fd) {
  int sockets[2];
  require(socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC | SOCK_NONBLOCK, 0, sockets) == 0,
          "media_spawn", "Cannot create private media channel");
  socket_ = sockets[0];
  posix_spawn_file_actions_t actions;
  posix_spawn_file_actions_init(&actions);
  posix_spawn_file_actions_addclose(&actions, sockets[0]);
  posix_spawn_file_actions_adddup2(&actions, sockets[1], 3);
  if (sockets[1] != 3) posix_spawn_file_actions_addclose(&actions, sockets[1]);
  char* args[] = {const_cast<char*>(worker_.c_str()), const_cast<char*>("--launch-media"),
                  const_cast<char*>(cached_fd >= 0 || !playback_.online() ? "--offline" : playback_.global_policy().at("localNetwork") == true ? "--lan" : "--public"), nullptr};
  auto rc = posix_spawn(&pid_, worker_.c_str(), &actions, nullptr, args, environ);
  posix_spawn_file_actions_destroy(&actions);
  close(sockets[1]);
  if (rc != 0) {
    close(socket_);
    socket_ = -1;
    throw Error("media_spawn", "Cannot launch media worker: " + std::to_string(rc));
  }
  try {
    auto bounds = presentation_.bounds(occurrence_.display);
    send_media(
        socket_,
        {{"method", "initialize"},
         {"source", source_override_.empty() ? occurrence_.item["source"] : Json(source_override_)},
         {"rasterImage", native_raster(occurrence_.item)},
         {"httpUserAgent", user_agent_},
         {"width", bounds.width},
         {"height", bounds.height},
         {"display", occurrence_.display},
         {"displayRevision", occurrence_.display_revision},
         {"paused", playback_.media_paused(id)},
         {"volume", playback_.state()["settings"].value("volume", 1.0)},
         {"muted", playback_.state()["settings"].value("muted", false)}},
        cached_fd >= 0 ? std::vector<int>{cached_fd} : std::vector<int>{});
  } catch (...) {
    stop_worker();
    throw;
  }
}
MediaClient::~MediaClient() {
  if (artwork_cache_) artwork_cache_->retire(id);
  if (cache_) cache_->active = false;
  stop_worker();
  presentation_.destroy(frames.capture);
  presentation_.destroy(frames.shown);
}
bool MediaClient::fallback_live() {
  if (!mixed_cache_ || !playback_.online() || virtual_cache_source(occurrence_.item.at("source")))
    return false;
  mixed_cache_ = false;
  start_live();
  received_ = std::chrono::steady_clock::now();
  return true;
}
void MediaClient::poll(bool hold, const Json* stage,bool allow_direct) {
  reap_media_workers();
  if (failed_) return;
  try {
    if (cache_ && pid_ < 0) {
      auto replies = cache_->take();
      if (replies.empty()) {
        require(std::chrono::steady_clock::now() - received_ < std::chrono::seconds(31),
                "cache_timeout", "Cached media did not become available");
        return;
      }
      auto& reply = replies.front();
      if (!reply.error.empty() && fallback_live()) return;
      require(reply.error.empty(), "cache_unavailable", reply.error);
      auto path = reply.metadata.at("path").get<std::string>();
      int fd = open(path.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
      if (fd < 0 && fallback_live()) return;
      require(fd >= 0, "cache_unavailable", "Cached media was removed before it could be opened");
      struct stat info{};
      if (fstat(fd, &info) != 0 || !S_ISREG(info.st_mode) ||
          info.st_size != reply.metadata.at("size")) {
        close(fd);
        if (fallback_live()) return;
        throw Error("cache_integrity", "Cached media inode or size changed");
      }
      try {
        start(fd);
      } catch (...) {
        close(fd);
        throw;
      }
      close(fd);
      received_ = std::chrono::steady_clock::now();
    }
    for (int n = 0; n < 64; ++n) {
      auto packet = receive_media(socket_);
      if (!packet) break;
      received_ = std::chrono::steady_clock::now();
      const auto& p = packet->body;
      auto event = string_field(p, "event", 32);
      if (event == "loaded") {
        loaded_ = true;
        playback_.committed(id);
      } else if (event == "frame") {
        DmaFrame info{};
        auto format = p.at("format").get<uint32_t>();
        require(format == DRM_FORMAT_ARGB8888 || format == DRM_FORMAT_ABGR8888, "media_protocol",
                "Unsupported media frame format");
        info.format = format;
        info.modifier = p.at("modifier").get<uint64_t>();
        const auto& planes = p.at("planes");
        require(planes.is_array() && !planes.empty() && planes.size() <= 4 &&
                    planes.size() == packet->descriptors.size(),
                "media_protocol", "Invalid media planes");
        info.count = planes.size();
        for (size_t i = 0; i < planes.size(); ++i) {
          info.planes[i].fd = packet->descriptors[i];
          info.planes[i].stride = planes[i].at("stride").get<uint32_t>();
          info.planes[i].offset = planes[i].at("offset").get<uint64_t>();
        }
        int width = p.at("width").get<int>(), height = p.at("height").get<int>();
        require(width > 0 && height > 0 && width <= 8192 && height <= 8192, "media_protocol",
                "Invalid frame size");
        info.width = width;
        info.height = height;
        presentation_.accelerated(frames.capture, info);
        frames.captured();
        frames.capture.display_revision = p.at("displayRevision").get<uint64_t>();
        received_sequence_ = p.at("sequence").get<uint64_t>();
        send_media(socket_, {{"method", "ack"}, {"sequence", p.at("sequence")}});
        // At most one decoded frame per host swap; don't drain and discard
        // future video frames while servicing a burst of decoder messages.
        break;
      } else if (event == "ended")
        playback_.ended(id);
      else if (event == "error")
        throw Error("media_backend", string_field(p, "message", 1024));
      else if(event=="pointer"){
        if(playback_.visible() && playback_.visible()->id==id) {
          pointer_motion=Json{{"kind","move"},{"x",p.at("x")},{"y",p.at("y")}};
          pointer_clicked |= p.value("clicked",false);
        }
      }else if (event == "property")
        status[string_field(p, "name", 64)] = p.at("value");
      else
        require(event == "initialized" || event == "heartbeat", "media_protocol",
                "Unexpected media event");
    }
    controls(hold, stage,allow_direct);
    const auto& surface = frames.latest();
    ready =
        loaded_ && surface.frames > 0 && surface.display_revision == render_revision_;
    require(std::chrono::steady_clock::now() - received_ < std::chrono::seconds(5),
            "media_unresponsive", "Media worker stopped responding for 5 seconds");
    status["backend"] = "native-media";
    status["workerPid"] = pid_;
    status["importedFrames"] = frames.count;
    status["surfaceWidth"] = surface.width;
    status["surfaceHeight"] = surface.height;
    status["workerResponsive"] = true;
    playback_.media_status(id, status);
    int exit_status = 0;
    auto ended = waitpid(pid_, &exit_status, WNOHANG);
    if (ended == pid_) {
      pid_ = -1;
      throw Error("media_exit", "Media worker exited: " + std::to_string(exit_status));
    }
  } catch (const std::exception& error) {
    fail(error.what());
  }
}
void MediaClient::stop_worker() {
  if (pid_ > 0) {
    kill(pid_, SIGKILL);
    retiring.push_back(pid_);
    pid_ = -1;
  }
  if (socket_ >= 0) {
    close(socket_);
    socket_ = -1;
  }
}
void MediaClient::fail(const std::string& message) {
  stop_worker();
  ready = false;
  if (!local_media_key_.empty()) {
    // A separately fetched representation can pass HTTP validation and
    // still fail to decode. Give this occurrence one normal live attempt.
    ArtworkCache::discard_media(local_media_key_);
    local_media_key_.clear();
    if (playback_.online()) {
      presentation_.destroy(frames.capture);
      presentation_.destroy(frames.shown);
      frames = {};
      loaded_ = false;
      controls_.clear();
      status = Json::object();
      presented_sequence_ = received_sequence_ = 0;
      received_ = std::chrono::steady_clock::now();
      try {
        start();
        return;
      } catch (const std::exception& error) {
        failed_ = true;
        playback_.failed(id, error.what());
        return;
      }
    }
  }
  failed_ = true;
  playback_.failed(id, message);
}
void MediaClient::controls(bool hold, const Json* stage,bool allow_direct) {
  if (playback_.desired() && playback_.desired()->id == id) {
    occurrence_.display = playback_.desired()->display;
    occurrence_.display_revision = playback_.desired()->display_revision;
  } else if (playback_.visible() && playback_.visible()->id == id) {
    occurrence_.display = playback_.visible()->display;
    occurrence_.display_revision = playback_.visible()->display_revision;
  }
  auto display = occurrence_.display;
  if (stage) {
    for (auto key : {"margin", "scaling"}) display[key] = stage->at(key);
  }
  // The staging bit is private to the worker IPC. A staged texture cannot
  // satisfy a request for the occurrence's final display revision.
  render_revision_ = occurrence_.display_revision | (stage ? (uint64_t{1} << 63) : 0);
  auto bounds = presentation_.bounds(display);
  Json value = {{"method", "controls"},
                {"directOutput", allow_direct && !hold && !stage && presentation_.can_present_direct(occurrence_,playback_)},
                {"paused", hold || playback_.media_paused(id)},
                {"volume", playback_.state()["settings"].value("volume", 1.0)},
                {"muted", playback_.state()["settings"].value("muted", false)},
                {"display", display},
                {"displayRevision", render_revision_},
                {"width", bounds.width},
                {"height", bounds.height}};
  auto key = value.dump();
  if (key != controls_) {
    send_media(socket_, value);
    controls_ = key;
  }
}
void MediaClient::seek(double seconds) {
  require(!failed_, "media_unavailable", "Media worker has failed");
  send_media(socket_, {{"method", "controls"}, {"seekSeconds", seconds}});
}
void MediaClient::did_present() {
  if (failed_ || received_sequence_ == presented_sequence_) return;
  try {
    send_media(socket_, {{"method", "presented"}, {"sequence", received_sequence_}});
    presented_sequence_ = received_sequence_;
    frames.submitted();
    if (warm_request_ && playback_.online() && playback_.desired() && playback_.desired()->id == id) {
      artwork_cache_->warm(id, *warm_request_);
      warm_request_.reset();
    }
  } catch (const std::exception& error) {
    fail(error.what());
  }
}
}  // namespace ff
