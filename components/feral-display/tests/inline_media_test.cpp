#include "ff/inline_media.hpp"

#include <fcntl.h>
#include <unistd.h>

#include <iostream>

#include "ff/json.hpp"
int main() {
  try {
    for (const auto& source : {"data:image/png;base64,AAEC/w==", "data:image/png,%00%01%02%ff"}) {
      int fd = ff::inline_media_fd(source);
      char bytes[4];
      ff::require(read(fd, bytes, 4) == 4 && bytes[0] == 0 && bytes[1] == 1 && bytes[2] == 2 &&
                      uint8_t(bytes[3]) == 255,
                  "test", "Inline bytes preserved");
      ff::require(fcntl(fd, F_GET_SEALS) & F_SEAL_WRITE, "test", "Inline file is immutable");
      close(fd);
    }
    for (const auto& source :
         {"data:image/png;base64,A===", "data:image/png;base64,????", "data:image/png,%ff%xx",
          "data:image/png,", "https://example.org/"}) {
      bool rejected = false;
      try {
        int fd = ff::inline_media_fd(source);
        close(fd);
      } catch (const ff::Error&) {
        rejected = true;
      }
      ff::require(rejected, "test", "Malformed inline data must be rejected");
    }
    std::cout << "Inline media binary preservation, sealing and malformed URL checks passed\n";
  } catch (const std::exception& e) {
    std::cerr << e.what() << '\n';
    return 1;
  }
}
