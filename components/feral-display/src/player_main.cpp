// Load only the role needed in this process. In particular, the limited-address-
// space media decoder must not map Chromium or initialize the Go runtime.
#include <dlfcn.h>
#include <filesystem>
#include <iostream>
#include <string_view>

int launch_sandbox(int argc, char** argv);
__attribute__((no_stack_protector)) int main(int argc, char** argv) {
  std::string_view role = argc > 1 ? argv[1] : "";
  const char* library = "libff_host.so";
  const char* symbol = "ff_host";
  if (role == "--launch-browser" || role == "--launch-media") return launch_sandbox(argc, argv);
  if (role == "host" || role == "host-lan" || role == "inside" ||
      role == "serve" || role == "serve-lan" || role == "relay") {
    library = "libff_network.so";
    symbol = "ff_network";
  } else if (role == "--decode") {
    library = "media/libff_decode.so";
    symbol = "ff_decode";
  } else if (role == "--wayland-relay") {
    library = "libff_wayland.so";
    symbol = "ff_wayland";
  } else {
    for (int i = 1; i < argc; ++i) {
      if (std::string_view(argv[i]) == "--cef" || std::string_view(argv[i]).starts_with("--type=")) {
        library = "libff_browser.so";
        symbol = "ff_browser";
        break;
      }
    }
  }
  const auto path = std::filesystem::canonical("/proc/self/exe").parent_path() / library;
  void* module = dlopen(path.c_str(), RTLD_NOW | RTLD_LOCAL);
  auto entry = module ? reinterpret_cast<int (*)(int,char**)>(dlsym(module, symbol)) : nullptr;
  if (!entry) {
    std::cerr << dlerror() << '\n';
    return 1;
  }
  return entry(argc, argv);
}
