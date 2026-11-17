#pragma once
#include "ff/protocol.hpp"
#include <vector>
namespace ff {
// Nonblocking bounded I/O on the host loop. No client can hold a UI callback
// waiting for more bytes or block another client while it consumes a reply.
class Server {
 public:
  Server(const std::string& path, Protocol& protocol, std::function<int64_t()> clock = {});
  ~Server();
  void poll();
 private:
  struct Client {
    int fd;
    Connection protocol;
    std::string input, output;
    size_t sent = 0;
    int64_t connected = 0, reading = 0, writing = 0;
  };
  int listener_ = -1, lock_ = -1;
  std::string path_;
  Protocol& protocol_;
  std::function<int64_t()> clock_;
  std::vector<Client> clients_;
  bool service(Client& client);
};
}
