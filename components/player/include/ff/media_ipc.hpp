#pragma once
#include "ff/json.hpp"
#include <optional>
#include <vector>

namespace ff {
// Private SOCK_SEQPACKET channel inherited by exactly one media worker.
// Frames carry DMA-BUF descriptors, never pixel bytes. Acknowledgement permits
// reuse only after the host has completed its GPU copy.
constexpr size_t media_packet_limit = 65536;
struct MediaPacket {
  Json body;
  std::vector<int> descriptors;
  MediaPacket() = default;
  MediaPacket(MediaPacket&& other) noexcept;
  ~MediaPacket();
  MediaPacket(const MediaPacket&) = delete;
};
void send_media(int socket, const Json& body, const std::vector<int>& descriptors = {});
std::optional<MediaPacket> receive_media(int socket);
}
