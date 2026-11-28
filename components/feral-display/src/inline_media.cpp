#include "ff/inline_media.hpp"

#include <fcntl.h>
#include <sys/mman.h>
#include <unistd.h>

#include "ff/json.hpp"
namespace ff {
int inline_media_fd(const std::string& source) {
  require(source.starts_with("data:") && source.size() <= 16384, "media_source",
          "Inline media exceeds URL limit");
  auto comma = source.find(',');
  require(comma != std::string::npos && comma <= 256, "media_source",
          "Invalid inline media declaration");
  std::string data;
  auto encoded = source.substr(comma + 1);
  if (source.substr(0, comma).ends_with(";base64")) {
    require(encoded.size() % 4 == 0, "media_source", "Invalid base64 media length");
    const std::string alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    for (size_t i = 0; i < encoded.size(); i += 4) {
      uint32_t value = 0;
      int padding = 0;
      for (size_t n = 0; n < 4; ++n) {
        auto c = encoded[i + n];
        if (c == '=') {
          require(i + 4 == encoded.size() && n >= 2, "media_source", "Invalid base64 padding");
          ++padding;
          value <<= 6;
        } else {
          require(padding == 0, "media_source", "Invalid base64 padding order");
          auto index = alphabet.find(c);
          require(index != std::string::npos, "media_source", "Invalid base64 media character");
          value = (value << 6) | index;
        }
      }
      require(padding <= 2, "media_source", "Invalid base64 padding");
      data.push_back(char(value >> 16));
      if (padding < 2) data.push_back(char(value >> 8));
      if (padding < 1) data.push_back(char(value));
    }
  } else {
    auto hex = [](unsigned char c) -> int {
      if (c >= '0' && c <= '9') return c - '0';
      if (c >= 'a' && c <= 'f') return c - 'a' + 10;
      if (c >= 'A' && c <= 'F') return c - 'A' + 10;
      return -1;
    };
    for (size_t i = 0; i < encoded.size(); ++i) {
      if (encoded[i] == '%') {
        require(i + 2 < encoded.size(), "media_source", "Truncated percent escape");
        int a = hex(encoded[i + 1]), b = hex(encoded[i + 2]);
        require(a >= 0 && b >= 0, "media_source", "Invalid percent escape");
        data.push_back(char(a * 16 + b));
        i += 2;
      } else
        data.push_back(encoded[i]);
    }
  }
  require(!data.empty(), "media_source", "Empty inline media");
  int fd = memfd_create("ff-inline-media", MFD_CLOEXEC | MFD_ALLOW_SEALING);
  require(fd >= 0, "media_source", "Cannot create inline media file");
  size_t offset = 0;
  while (offset < data.size()) {
    auto n = write(fd, data.data() + offset, data.size() - offset);
    if (n <= 0) {
      close(fd);
      throw Error("media_source", "Cannot write inline media");
    }
    offset += n;
  }
  if (lseek(fd, 0, SEEK_SET) < 0 ||
      fcntl(fd, F_ADD_SEALS, F_SEAL_WRITE | F_SEAL_GROW | F_SEAL_SHRINK | F_SEAL_SEAL) < 0) {
    close(fd);
    throw Error("media_source", "Cannot seal inline media");
  }
  return fd;
}
}  // namespace ff
