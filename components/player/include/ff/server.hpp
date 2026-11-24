#pragma once
#include "ff/protocol.hpp"
#include <chrono>
#include <condition_variable>
#include <mutex>
#include <thread>
#include <vector>
namespace ff {
// Nonblocking bounded I/O on the host loop. No client can hold a UI callback
// waiting for more bytes or block another client while it consumes a reply.
class Server {
 public:
  enum class Phase { frame, swap };
  Server(const std::string& path, Protocol& protocol, std::function<int64_t()> clock = {});
  ~Server();
  void poll(Phase phase = Phase::frame);
 private:
  struct Client {
    int fd;
    Connection protocol;
    std::string input, output;
    size_t sent = 0;
    int64_t connected = 0, reading = 0, writing = 0;
    bool waits_for_frame = false;
  };
  int listener_ = -1, lock_ = -1;
  std::string path_;
  Protocol& protocol_;
  std::function<int64_t()> clock_;
  std::vector<Client> clients_;
  bool service(Client& client, Phase phase);
};

// The host normally owns both Playback and Server. While its graphics thread
// is inside submit(), this thread may run the same command dispatcher. Joining
// the last poll before returning restores exclusive ownership to the frame loop.
class SwapControl {
 public:
  SwapControl(Server& server, Playback& playback, std::chrono::milliseconds progress_limit = std::chrono::seconds(20));
  ~SwapControl();
  int64_t poll(Server::Phase phase = Server::Phase::frame);
  // False means a command changed the frame's state during submission.
  bool submit(const std::function<void()>& swap);
 private:
  void serve();
  Server& server_;
  Playback& playback_;
  const std::chrono::milliseconds progress_limit_;
  std::mutex mutex_;
  std::condition_variable changed_;
  std::optional<std::chrono::steady_clock::time_point> started_;
  bool stopping_ = false;
  std::exception_ptr failure_;
  std::thread worker_;
};
}
