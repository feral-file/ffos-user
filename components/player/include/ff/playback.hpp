#pragma once
#include "ff/store.hpp"
#include <functional>
#include <optional>

namespace ff {
struct Occurrence {
  std::string id;
  Json item;
  Json display;
  int index = 0;
  uint64_t generation = 0;
  std::string stage = "loading";
  std::string error;
  bool navigation_committed = false;
  Json defaults = Json::object();
  std::string context = "curated", playlist_title, curator;
  // Retain the admitted document for the lifetime of this occurrence. A new
  // cast can load while this occurrence is still on screen.
  Json playlist;
  std::string playlist_url;
  // Stable across source refreshes; changes for an explicit cast. Unlike the
  // command generation this identifies outgoing occurrences of this cast.
  std::string cast_id;
  bool frame_available = false; // Host capture evidence, never persisted.
  Json adjustment = Json::object();
  uint64_t display_revision = 1;
};

// Called only on the host event loop. The renderer consumes desired() and
// returns occurrence-scoped observations; it never receives command authority.
class Playback {
 public:
  explicit Playback(Store& store);
  Json command(std::string_view method, const Json& params);
  Json snapshot() const;
  void tick(int64_t monotonic_ms, int64_t wall_time_ms);
  void committed(const std::string& occurrence);
  void frame_ready(const std::string& occurrence);
  void presented(const std::string& occurrence);
  void composed(const std::string& occurrence,uint64_t display_revision);
  void failed(const std::string& occurrence, std::string reason);
  void ended(const std::string& occurrence);
  const std::optional<Occurrence>& desired() const { return selected_; }
  const std::optional<Occurrence>& visible() const { return presented_; }
  const Json& state() const { return state_; }
  uint64_t generation() const { return generation_; }
  uint64_t revision() const { return revision_; }
  // With an occurrence, includes the decoder's initial presentation hold.
  // Without one, reports the user's pause/autoplay intent for status.
  bool media_paused(const std::string& occurrence = {}) const;
  const std::string& instance() const { return instance_; }
  bool online() const { return online_; }
  Json overlay() const { return overlay_; }
  const Json& composition() const { return composition_; }
  bool composition_pending() const { return presented_&&!composition_.is_null()&&composition_.value("occurrence","")==presented_->id&&composed_display_revision_!=presented_->display_revision; }
  const std::string& storage_error() const { return storage_error_; }
  const std::string& recovery_error() const { return recovery_error_; }
  const Json& presentation_failure() const { return presentation_failure_; }
  void set_input_handler(std::function<void(const Json&)> fn) { input_ = std::move(fn); }
  void set_seek_handler(std::function<void(const std::string&, double)> fn) { seek_ = std::move(fn); }
  void media_status(const std::string& occurrence, const Json& status);
  void release_input();
 private:
  Store& store_;
  Json state_;
  std::string instance_ = uuid();
  std::string cast_id_ = uuid();
  uint64_t generation_ = 1, revision_ = 1;
  std::optional<Occurrence> selected_, presented_;
  Json overlay_ = nullptr;
  bool online_ = false;
  bool autoplay_hold_ = false;
  bool eos_pending_ = false;
  int64_t now_ = 0, selected_at_ = 0, presented_at_ = 0, pause_at_ = 0;
  bool clock_started_ = false;
  std::function<void(const Json&)> input_;
  std::function<void(const std::string&, double)> seek_;
  Json media_status_ = nullptr;
  Json presentation_failure_ = nullptr;
  Json composition_ = nullptr;
  uint64_t composition_revision_ = 0;
  uint64_t composed_display_revision_ = 0;
  int64_t media_status_at_ = 0;
  std::string storage_error_;
  int64_t storage_retry_at_ = 0;
  // Retry budget belongs to one selection, not one renderer. Short-lived
  // successful frames cannot hide a crash loop by replenishing it.
  unsigned recovery_attempt_ = 0;
  int64_t recovery_at_ = 0;
  std::string recovery_error_, recovery_occurrence_;
  void clear_recovery();
  void persist(Json next);
  void select(bool recovering = false);
  void advance();
  void execute_schedule(int64_t wall_time_ms);
  void replace(Json& next, const Json& params);
  Json update_playlist(const Json& params);
  bool apply_pending_update();
  void update_order(Json& next, int index) const;
  Json display(const Json& item, const Json& defaults, const Json& adjustment = Json::object()) const;
  bool allowed(const Json& item, const Json& state) const;
  void validate_playlist(const Json& playlist) const;
  void validate_state() const;
  double duration() const;
};
}
