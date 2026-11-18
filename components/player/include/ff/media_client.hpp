#pragma once
#include "ff/presentation.hpp"
#include "ff/media_kind.hpp"
#include <sys/types.h>
#include <chrono>

namespace ff {
// Reap only retired media children, even when no MediaClient remains. Never
// consume CEF's child exit statuses with a process-wide waitpid(-1).
void reap_media_workers();
class MediaClient {
 public:
  MediaClient(Playback& playback, Presentation& presentation, Occurrence occurrence, const std::string& worker);
  ~MediaClient();
  void poll();
  void controls();
  void seek(double seconds);
  void did_present();
  FramePair frames;
  bool ready=false;
  Json status=Json::object();
  const std::string id;
 private:
  Playback& playback_;
  Presentation& presentation_;
  Occurrence occurrence_;
  int socket_=-1;
  pid_t pid_=-1;
  bool failed_=false,loaded_=false;
  std::string controls_;
  uint64_t received_sequence_=0,presented_sequence_=0;
  std::chrono::steady_clock::time_point received_ = std::chrono::steady_clock::now();
  void fail(const std::string& message);
};
}
