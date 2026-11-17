#include "ff/protocol.hpp"

namespace ff {
std::string frame(const Json& value) {
  auto body = value.dump();
  require(!body.empty() && body.size() <= max_frame, "frame_size", "Reply exceeds frame limit");
  uint32_t n = static_cast<uint32_t>(body.size());
  std::string output(4, '\0');
  for (int i = 0; i < 4; ++i) output[i] = static_cast<char>(n >> ((3 - i) * 8));
  return output + body;
}
Json Protocol::receive(Connection& connection, const Json& r) {
  Json id = nullptr;
  try {
    fields(r, {"v", "id", "method", "params", "instance", "generation", "ifRevision", "deadlineMs", "key"});
    id = string_field(r, "id", 64);
    require(r.contains("v") && r["v"].is_number_integer() && r["v"] == 1, "version_mismatch", "Supported protocol major: 1");
    auto method = string_field(r, "method", 64);
    const auto p = r.value("params", Json::object());
    require(p.is_object(), "invalid_params", "params must be an object");
    Json result;
    if (method == "hello") {
      fields(p, {"client", "versions"});
      auto client = string_field(p, "client", 64);
      require(p.contains("versions") && p["versions"].is_array() && !p["versions"].empty() && p["versions"].size() <= 16, "version_mismatch", "Supply supported protocol versions");
      bool compatible = false;
      for (const auto& v : p["versions"]) {
        require(v.is_number_integer() && v > 0 && v <= 65535, "version_mismatch", "Protocol versions must be positive integers");
        compatible |= v == 1;
      }
      require(compatible, "version_mismatch", "No common protocol version");
      connection.client = std::move(client);
      connection.negotiated = true;
      result = {{"version", 1}, {"instance", playback_.instance()}, {"maxFrameBytes", max_frame}, {"receiptWindow", 1024},
                {"capabilities", {"playback", "playback.update.v1", "settings", "overlays", "overlay.setup.v1", "history", "schedule", "input", "snapshots"}}, {"snapshot", playback_.snapshot()}};
    } else {
      require(connection.negotiated, "not_negotiated", "Send hello first");
      const bool query = method == "state.get" || method == "health.get" || method == "history.list" || method == "history.resolve" || method == "policy.get" || method == "state.subscribe";
      require(r.contains("deadlineMs") && r["deadlineMs"].is_number_integer(), "invalid_params", "deadlineMs is required");
      auto deadline = r["deadlineMs"].get<int64_t>(); auto now = wall_ms();
      require(deadline >= now && deadline <= now + 30000, "deadline_exceeded", "Deadline expired or exceeds 30 seconds");
      std::string receipt_key, fingerprint;
      if (!query) {
        require(r.value("instance", "") == playback_.instance(), "stale_instance", "Runtime restarted; negotiate and reconcile before retrying");
        auto key = string_field(r, "key", 64);
        receipt_key = Json::array({connection.client,key}).dump();
        auto canonical = r; canonical.erase("id"); canonical.erase("deadlineMs");
        fingerprint = canonical.dump();
        auto prior = receipts_.find(receipt_key);
        if (prior != receipts_.end()) {
          require(prior->second.fingerprint == fingerprint, "idempotency_conflict", "Key reused with a different operation");
          auto response = prior->second.response; response["id"] = id; return response;
        }
        require(r.contains("generation") && r["generation"].is_number_unsigned() && r["generation"].get<uint64_t>() == playback_.generation(), "stale_generation", "Playback generation changed");
        if (r.contains("ifRevision")) require(r["ifRevision"].is_number_unsigned() && r["ifRevision"].get<uint64_t>() == playback_.revision(), "stale_revision", "State changed; reconcile snapshot");
      }
      if (method == "state.subscribe") {
        fields(p, {}); connection.subscribed = true; connection.last_event = playback_.revision();
        result = {{"snapshot", playback_.snapshot()}, {"cursor", connection.last_event}};
      } else {
        if (method == "input.dispatch") require(input_connection_.empty() || input_connection_ == connection.token, "input_busy", "Another connection owns input");
        result = playback_.command(method, p);
        if (method == "input.dispatch") input_connection_ = p.value("kind", "") == "releaseAll" ? "" : connection.token;
      }
      Json response = {{"v", 1}, {"id", id}, {"result", result}};
      if (!query) {
        const auto bytes = fingerprint.size() + response.dump().size();
        while (!receipt_order_.empty() && (receipts_.size() >= 1024 || receipt_bytes_ + bytes > 8 * max_frame)) {
          const auto& oldest = receipts_.at(receipt_order_.front());
          receipt_bytes_ -= oldest.fingerprint.size() + oldest.response.dump().size();
          receipts_.erase(receipt_order_.front()); receipt_order_.pop_front();
        }
        receipts_.emplace(receipt_key, Receipt{fingerprint, response}); receipt_order_.push_back(receipt_key);
        receipt_bytes_ += bytes;
      }
      return response;
    }
    return {{"v", 1}, {"id", id}, {"result", result}};
  } catch (const Error& e) {
    return {{"v", 1}, {"id", id}, {"error", {{"code", e.code}, {"message", e.what()}}}};
  } catch (const Json::exception&) {
    return {{"v", 1}, {"id", id}, {"error", {{"code", "invalid_params"}, {"message", "Invalid field type or numeric range"}}}};
  }
}
void Protocol::disconnected(const Connection& connection) {
  if (input_connection_ == connection.token) { playback_.release_input(); input_connection_.clear(); }
}
std::optional<Json> Protocol::event(Connection& connection) {
  if (!connection.subscribed || connection.last_event == playback_.revision()) return std::nullopt;
  auto prior = connection.last_event; connection.last_event = playback_.revision();
  // Every event is a complete snapshot. Coalescing can skip revisions without
  // losing state; consumers detect that via previous and never apply a delta.
  return Json{{"v", 1}, {"event", "state"}, {"previous", prior}, {"cursor", connection.last_event}, {"snapshot", playback_.snapshot()}};
}
}
