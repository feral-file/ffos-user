#pragma once
#include "ff/playback.hpp"
#include <deque>
#include <unordered_map>

namespace ff {
struct Connection {
  std::string token = uuid();
  bool negotiated = false;
  bool subscribed = false;
  std::string client;
  uint64_t last_event = 0;
};
class Protocol {
 public:
  explicit Protocol(Playback& playback, bool artwork_permissions = true)
      : playback_(playback), artwork_permissions_(artwork_permissions) {}
  Json receive(Connection& connection, const Json& request);
  std::optional<Json> event(Connection& connection);
  void disconnected(const Connection& connection);
 private:
  Playback& playback_;
  const bool artwork_permissions_;
  struct Receipt { std::string fingerprint; Json response; };
  std::unordered_map<std::string, Receipt> receipts_;
  std::deque<std::string> receipt_order_;
  size_t receipt_bytes_ = 0;
  std::string input_connection_;
};
std::string frame(const Json& value);
}
