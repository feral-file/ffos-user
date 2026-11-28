#include "ff/media_ipc.hpp"
#include <sys/socket.h>
#include <unistd.h>
#include <cerrno>
#include <cstring>

namespace ff {
MediaPacket::MediaPacket(MediaPacket&& other) noexcept : body(std::move(other.body)), descriptors(std::move(other.descriptors)) { other.descriptors.clear(); }
MediaPacket::~MediaPacket() { for (int fd : descriptors) close(fd); }
void send_media(int socket, const Json& body, const std::vector<int>& descriptors) {
  auto data = body.dump();
  require(data.size() <= media_packet_limit && descriptors.size() <= 4, "media_protocol", "Media packet exceeds bounds");
  iovec io{data.data(), data.size()}; msghdr message{}; message.msg_iov = &io; message.msg_iovlen = 1;
  alignas(cmsghdr) char control[CMSG_SPACE(sizeof(int)*4)]{};
  if (!descriptors.empty()) {
    message.msg_control = control; message.msg_controllen = CMSG_SPACE(sizeof(int)*descriptors.size());
    auto header = CMSG_FIRSTHDR(&message); header->cmsg_level = SOL_SOCKET; header->cmsg_type = SCM_RIGHTS;
    header->cmsg_len = CMSG_LEN(sizeof(int)*descriptors.size());
    std::memcpy(CMSG_DATA(header), descriptors.data(), sizeof(int)*descriptors.size());
  }
  require(sendmsg(socket, &message, MSG_NOSIGNAL | MSG_DONTWAIT) == static_cast<ssize_t>(data.size()), "media_disconnected", "Media worker channel is closed or backpressured");
}
std::optional<MediaPacket> receive_media(int socket) {
  char buffer[media_packet_limit]; alignas(cmsghdr) char control[CMSG_SPACE(sizeof(int)*4)]{};
  iovec io{buffer, sizeof buffer}; msghdr message{}; message.msg_iov = &io; message.msg_iovlen = 1;
  message.msg_control = control; message.msg_controllen = sizeof control;
  auto size = recvmsg(socket, &message, MSG_DONTWAIT | MSG_CMSG_CLOEXEC);
  if (size < 0 && (errno == EAGAIN || errno == EINTR || errno == EWOULDBLOCK)) return std::nullopt;
  require(size > 0, "media_disconnected", "Media worker disconnected");
  MediaPacket packet;
  for (auto header = CMSG_FIRSTHDR(&message); header; header = CMSG_NXTHDR(&message, header)) {
    if (header->cmsg_level == SOL_SOCKET && header->cmsg_type == SCM_RIGHTS) {
      auto count = (header->cmsg_len - CMSG_LEN(0)) / sizeof(int);
      auto fds = reinterpret_cast<const int*>(CMSG_DATA(header));
      packet.descriptors.insert(packet.descriptors.end(), fds, fds+count);
    }
  }
  require(!(message.msg_flags & (MSG_TRUNC | MSG_CTRUNC)), "media_protocol", "Truncated media packet");
  packet.body = parse(std::string_view(buffer, size));
  return packet;
}
}
