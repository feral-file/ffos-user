#include "ff/capsule_engine.hpp"
#include <json.hpp>
#include <fstream>
#include <set>
#include <fcntl.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>
#include <algorithm>
#include <cerrno>
#include <ctime>
#include <filesystem>
#include <iostream>
#include <stdexcept>
#include <string>
#include <vector>

namespace {
namespace fs = std::filesystem;
using Args = std::vector<std::string>;

void append(Args& args, std::initializer_list<std::string> values) {
  args.insert(args.end(), values);
}
std::string env(const char* key) {
  const char* value = getenv(key);
  if (!value || !*value) throw std::runtime_error(std::string("missing ") + key);
  return value;
}
int execute(Args& args) {
  std::vector<char*> argv;
  for (auto& arg : args) argv.push_back(arg.data());
  argv.push_back(nullptr);
  execv(argv[0], argv.data());
  perror(argv[0]);
  return 1;
}
void bind_read_only(Args& args, const fs::path& path) {
  append(args, {"--ro-bind", path.string(), path.string()});
}

// The parent retains a lease while bwrap and the broker repurpose inherited FDs.
int browser_cache(Args& mounts, Args& flags, const std::string& key,
                  const char* max_bytes, const char* max_days) {
  if (key.size() != 64 || key.find_first_not_of("0123456789abcdef") != key.npos) return -1;
  fs::path dir = fs::path(env("HOME")) / ".cache/player-artworks" / key;
  std::error_code ec;
  fs::create_directories(dir, ec);
  if (ec) return -1;
  int fd = open(dir.c_str(), O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
  if (fd < 0) return -1;
  struct stat inode{}, current{};
  if (flock(fd, LOCK_EX | LOCK_NB) || fstat(fd, &inode) || lstat(dir.c_str(), &current) ||
      current.st_dev != inode.st_dev || current.st_ino != inode.st_ino ||
      time(nullptr) - inode.st_mtime >= std::stoll(max_days) * 86400 || futimens(fd, nullptr)) {
    close(fd);
    return -1;
  }
  append(mounts, {"--bind", dir.string(), "/artwork-cache"});
  append(flags, {"--disk-cache-dir=/artwork-cache",
                 "--disk-cache-size=" + std::to_string(std::min(67108864LL, std::stoll(max_bytes)))});
  return fd;
}
}

int launch_sandbox(int argc, char** argv) {
  try {
    const std::string self = fs::canonical("/proc/self/exe");
    const fs::path root = fs::path(self).parent_path();
    bool browser = std::string_view(argv[1]) == "--launch-browser";
    if ((browser && argc != 12 && argc != 13) || (!browser && argc != 3)) return 2;
    bool online = browser ? std::string_view(argv[3]) == "online" : std::string_view(argv[2]) != "--offline";
    bool lan = browser ? std::string_view(argv[6]) == "lan" : std::string_view(argv[2]) == "--lan";
    std::string mode = browser ? argv[5] : "";
    const bool pinned = browser && argc == 13;
    if (pinned && (online || mode != "nested" || !ff::valid_engine_digest(argv[12]))) return 2;
    if (mode == "cage") {
      Args relay{self, "--wayland-relay", self};
      relay.insert(relay.end(), argv + 1, argv + argc);
      relay[7] = "filtered";
      return execute(relay);
    }
    Args args;
    if (online) append(args, {self, lan ? "host-lan" : "host"});
    append(args, {"/usr/bin/bwrap", "--die-with-parent", "--new-session", "--unshare-all", "--clearenv"});
    const std::string profile_mount = pinned ? "/work" : "/profile";
    if (pinned) {
      append(args, {"--ro-bind", ff::capsule_engine_root(argv[12]).string(), "/",
                    "--tmpfs", "/sys", "--setenv", "HOME", profile_mount, "--setenv", "XDG_RUNTIME_DIR", profile_mount,
                    "--setenv", "PATH", "/usr/bin"});
    } else {
      append(args, {"--ro-bind", "/usr", "/usr", "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib", "/lib64"});
    }
    append(args, {"--proc", "/proc", "--dev", "/dev", "--dev-bind", "/dev/dri", "/dev/dri", "--tmpfs", "/tmp"});
    if (!pinned && !root.string().starts_with("/usr/")) bind_read_only(args, root);
    for (const char* path : {"/sys/devices", "/sys/dev/char", "/sys/class/drm", "/etc/ssl", "/etc/fonts",
                             "/etc/resolv.conf", "/etc/hosts", "/etc/ca-certificates"})
      if ((!pinned || std::string_view(path).starts_with("/sys/")) && fs::exists(path)) bind_read_only(args, path);
    append(args, {"--setenv", "LANG", "C.UTF-8"});
    Args child;
    if (online) append(child, {self, "inside"});
    int lease = -1;
    if (browser) {
      append(args, {"--bind", argv[2], profile_mount, "--setenv", "AMD_DEBUG", "nodcc"});
      auto index = fs::path(argv[2]) / "stream-index.json";
      if (fs::exists(index)) {
        // Each mount holds one immutable captured object. The store, other
        // artworks and the index's original writable name stay outside /streams.
        auto resources = nlohmann::json::parse(std::ifstream(index));
        std::set<std::string> mounted;
        append(args, {"--ro-bind", index.string(), "/streams/index.json",
                      "--ro-bind", index.string(), "/profile/stream-index.json"});
        for (const auto& resource : resources) {
          auto hash = resource.at("hash").get<std::string>();
          if (hash.size()!=64 || hash.find_first_not_of("0123456789abcdef")!=hash.npos) return 2;
          if (mounted.insert(hash).second)
            append(args, {"--ro-bind", (fs::path(argv[11]) / "blobs" / hash).string(), "/streams/" + hash});
        }
      }
      if (mode == "filtered") {
        fs::path relay = fs::path(env("XDG_RUNTIME_DIR")) / env("WAYLAND_DISPLAY");
        append(args, {"--ro-bind", relay.string(), "/display/wayland", "--setenv", "WAYLAND_DISPLAY", "/display/wayland"});
      } else {
        append(args, {"--setenv", "WAYLAND_DISPLAY", profile_mount + "/wayland"});
      }
      if (pinned) append(child, {"/usr/lib/chromium/chromium", "--start-fullscreen", "--noerrdialogs"});
      else append(child, {self, "--cef"});
      append(child, {"--ozone-platform=wayland", "--kiosk", "--no-first-run",
                     "--no-default-browser-check", "--disable-extensions", "--disable-background-networking",
                     "--disable-dev-shm-usage", "--deny-permission-prompts", "--test-type=gpu", "--enable-gpu",
                     "--enable-unsafe-webgpu",
                     "--user-data-dir=" + profile_mount, "--remote-debugging-pipe"});
      // Archive engines ship their own Mesa build and may have no Vulkan ICD.
      if (pinned) append(child, {"--use-gl=angle", "--use-angle=gl"});
      else append(child, {"--use-angle=vulkan", "--disable-vulkan-surface",
                          "--enable-features=Vulkan,VulkanFromANGLE,DefaultANGLEVulkan,VaapiIgnoreDriverChecks"});
      if (std::string_view(argv[4]) == "debug") append(child, {"--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1"});
      if (online) append(child, {"--proxy-server=http://127.0.0.1:17873", "--proxy-bypass-list=<-loopback>"});
      if (mode == "filtered") child.push_back("--ff-direct-cage");
      child.push_back(std::string_view(argv[7]) == "auto" ? "--autoplay-policy=no-user-gesture-required"
                                                          : "--autoplay-policy=user-gesture-required");
      lease = browser_cache(args, child, argv[8], argv[9], argv[10]);
      child.push_back("about:blank");
    } else {
      const auto runtime = env("XDG_RUNTIME_DIR");
      const std::string display = getenv("WAYLAND_DISPLAY") ? getenv("WAYLAND_DISPLAY") : "wayland-0";
      if (display.empty() || display.front() == '.' || display.find('/') != display.npos) return 2;
      bind_read_only(args, fs::path(runtime) / display);
      auto pulse = fs::path(runtime) / "pulse/native";
      if (fs::is_socket(pulse)) {
        bind_read_only(args, pulse);
        append(args, {"--setenv", "PULSE_SERVER", "unix:" + pulse.string()});
      }
      append(args, {"--setenv", "XDG_RUNTIME_DIR", runtime, "--setenv", "WAYLAND_DISPLAY", display,
                    "--setenv", "SDL_VIDEODRIVER", "wayland", "--setenv", "SDL_VIDEO_WAYLAND_ALLOW_LIBDECOR", "0",
                    "--setenv", "LD_LIBRARY_PATH", (root / "media").string(), "--setenv", "MALLOC_ARENA_MAX", "2",
                    "--setenv", "HOME", "/tmp"});
      // libmpv uses libcurl, which selects its proxy by URL scheme.
      if (online) append(args, {"--setenv", "http_proxy", "http://127.0.0.1:17873",
                                "--setenv", "https_proxy", "http://127.0.0.1:17873"});
      append(child, {self, "--decode"});
    }
    args.insert(args.end(), child.begin(), child.end());
    if (lease < 0) return execute(args);
    pid_t pid = fork();
    if (pid == 0) { close(lease); _exit(execute(args)); }
    int status = 0;
    pid_t result = -1;
    if (pid > 0) do { result = waitpid(pid, &status, 0); } while (result < 0 && errno == EINTR);
    close(lease);
    return result > 0 && WIFEXITED(status) ? WEXITSTATUS(status) : 1;
  } catch (const std::exception& error) {
    std::cerr << "Artwork sandbox: " << error.what() << '\n';
    return 1;
  }
}
