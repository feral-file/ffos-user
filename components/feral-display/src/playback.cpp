#include "ff/media_kind.hpp"
#include "ff/playback.hpp"
#include "ff/input.hpp"
#include "ff/overlay.hpp"
#include "ff/display.hpp"
#include <algorithm>
#include <cmath>
#include <chrono>
#include <numeric>
#include <random>

namespace ff {
namespace {
Json occurrence_json(const std::optional<Occurrence>& occurrence) {
  if (!occurrence) return nullptr;
  const auto& o = *occurrence;
  return {{"id", o.id}, {"generation", o.generation}, {"index", o.index}, {"item", o.item},
          {"castId", o.cast_id}, {"display", o.display}, {"displayRevision",o.display_revision}, {"stage", o.stage}, {"error", o.error}, {"navigationCommitted", o.navigation_committed},
          {"context",o.context},{"exhibition",o.playlist_title},{"curator",o.curator}};
}
bool boolean(const Json& p, const char* field) {
  require(p.contains(field) && p[field].is_boolean(), "invalid_params", std::string("Expected boolean: ") + field);
  return p[field].get<bool>();
}
void optional_text(const Json& p,const char* key,size_t limit=4096){
  if(p.contains(key))require(p[key].is_string()&&p[key].get_ref<const std::string&>().size()<=limit,
    "playlist_invalid",std::string("Invalid text field: ")+key);
}
void validate_display(const Json& p, bool restoring = false){
  require(p.is_object(),"playlist_invalid","Display must be an object");
  if(p.contains("userOverrides")){
    const auto& value=p.at("userOverrides");
    require(value.is_boolean() || (value.is_object() && std::all_of(value.begin(),value.end(),
      [](const Json& field){return field.is_boolean();})),"playlist_invalid","Invalid display override permissions");
  }
  if(p.contains("loop"))require(p["loop"].is_boolean(),"playlist_invalid","loop must be boolean");
  // Older hosts treated the lowercase field as unknown, including in history.
  if(!restoring && p.contains("autoplay"))require(p["autoplay"].is_boolean(),"playlist_invalid","autoplay must be boolean");
  if(p.contains("scaling"))require(p["scaling"]=="fit"||p["scaling"]=="fill"||p["scaling"]=="stretch"||p["scaling"]=="auto","playlist_invalid","Invalid display scaling");
  if(p.contains("imageRendering"))require(p["imageRendering"]=="auto" || p["imageRendering"]=="pixelated","playlist_invalid","Invalid image rendering");
  optional_text(p,"background",64);
  if(p.contains("background"))display_color(p["background"].get<std::string>());
  if(p.contains("margin")){
    if(p["margin"].is_number())require(p["margin"].get<double>()<=16384,"playlist_invalid","Authored pixel margin exceeds 16384");
    display_bounds(p,1920,1080);
  }
}
void validate_metadata(const Json& p){
  require(p.is_object(),"playlist_invalid","Metadata must be an object");
  optional_text(p,"title");
  if(p.contains("artists")){
    require(p["artists"].is_array()&&p["artists"].size()<=128,"playlist_invalid","Invalid artists");
    for(const auto& artist:p["artists"]){
      require(artist.is_string()||artist.is_object(),"playlist_invalid","Invalid artist");
      if(artist.is_object())optional_text(artist,"name");
      else require(artist.get_ref<const std::string&>().size()<=4096,"playlist_invalid","Artist name too long");
    }
  }
}
void validate_layer(const Json& p, bool restoring){
  require(p.is_object(),"playlist_invalid","Manifest/defaults/override must be an object");
  if(p.contains("display"))validate_display(p["display"], restoring);
  if(p.contains("metadata"))validate_metadata(p["metadata"]);
  if(p.contains("controls")){
    require(p["controls"].is_object(),"playlist_invalid","Manifest controls must be an object");
    if(p["controls"].contains("display"))validate_display(p["controls"]["display"], restoring);
  }
}
void validate_settings(const Json& p) {
  fields(p, {"scaling", "tombstone", "transitionMs", "durationSeconds", "margin", "background", "rotation", "volume", "muted", "framing"});
  if (p.contains("scaling")) require(p["scaling"] == "fit" || p["scaling"] == "fill" || p["scaling"] == "stretch" || p["scaling"] == "auto", "invalid_params", "Unknown scaling");
  if (p.contains("tombstone")) require(p["tombstone"] == "timed" || p["tombstone"] == "on" || p["tombstone"] == "off", "invalid_params", "Unknown label mode");
  for (const auto* key : {"transitionMs", "durationSeconds", "margin", "volume"}) {
    if (!p.contains(key) || (std::string(key) == "durationSeconds" && p[key].is_null())) continue;
    require(p[key].is_number() && p[key].get<double>() >= 0 && p[key].get<double>() <= 86400000, "invalid_params", "Invalid numeric setting");
  }
  if (p.contains("transitionMs")) require(p["transitionMs"].get<double>() <= 5000, "invalid_params", "Transition exceeds 5 seconds");
  if (p.contains("durationSeconds") && !p["durationSeconds"].is_null()) require(p["durationSeconds"].get<double>() > 0, "invalid_params", "Default duration must be positive or null for auto");
  if (p.contains("volume")) require(p["volume"].get<double>() <= 1, "invalid_params", "Volume must be 0..1");
  if (p.contains("muted")) boolean(p, "muted");
  if (p.contains("rotation")) require(p["rotation"] == 0 || p["rotation"] == 90 || p["rotation"] == 180 || p["rotation"] == 270, "invalid_params", "Invalid rotation");
  if (p.contains("background")) {
    auto c = string_field(p, "background", 9);
    require(c.size() == 7 && c[0] == '#' && c.find_first_not_of("0123456789abcdefABCDEF", 1) == std::string::npos, "invalid_params", "Background must be #RRGGBB");
  }
  if (p.contains("framing")) require(p["framing"]=="artwork"||p["framing"]=="fit"||p["framing"]=="fill", "invalid_params", "Framing must be artwork, fit or fill");
}
void validate_plan(const Json& plan) {
 fields(plan,{"playlist","context","playlistUrl","activationTimes","activeAt","started"});
 require(plan.contains("playlist")&&plan["playlist"].is_object()&&plan["playlist"].contains("items"),"invalid_params","Missing schedule playlist");
 const auto& times=plan.at("activationTimes");
 require(times.is_array()&&times.size()==plan["playlist"]["items"].size(),"invalid_params","activationTimes must match items");
 for(const auto& t:times)require(t.is_null()||t.is_number_integer(),"invalid_params","Invalid activation time");
 require(plan.value("context","curated")=="curated"||plan.value("context","curated")=="personal","invalid_params","Invalid schedule context");
 optional_text(plan,"playlistUrl",16384);
 if(plan.contains("started"))boolean(plan,"started");
 if(plan.contains("activeAt"))require(plan["activeAt"].is_null()||plan["activeAt"].is_number_integer(),"invalid_params","Invalid active cohort");
}
Json cohort(const Json& plan,int64_t wall,Json& active) {
 active=nullptr;
 for(const auto& t:plan["activationTimes"])if(!t.is_null()&&t.get<int64_t>()<=wall&&(active.is_null()||t.get<int64_t>()>active.get<int64_t>()))active=t;
 auto playlist=plan["playlist"];playlist["items"]=Json::array();
 for(size_t i=0;i<plan["activationTimes"].size();++i){const auto& t=plan["activationTimes"][i];if(t.is_null()||(!active.is_null()&&t==active))playlist["items"].push_back(plan["playlist"]["items"][i]);}
 return playlist;
}

}
Playback::Playback(Store& store) : store_(store), state_(store.load()) {
  // Validate before exposing a socket. Corruption must not start an empty cast.
  auto expected = initial_state();
  for (auto it = expected.begin(); it != expected.end(); ++it)
    require(state_.contains(it.key()) && (it.value().is_null() || (it.value().is_number_integer() && state_[it.key()].is_number_integer()) || state_[it.key()].type() == it.value().type()), "storage_corrupt", "Invalid persistent state field: " + it.key());
  if (!state_["playlist"].is_null()) validate_playlist(state_["playlist"], true);
  validate_settings(state_["settings"]);
  validate_state();
  if(state_.contains("cohortPlan")&&!state_["cohortPlan"].is_null()) { validate_plan(state_["cohortPlan"]);validate_playlist(state_["cohortPlan"]["playlist"], true); }
  if(state_.contains("pendingUpdate")&&!state_["pendingUpdate"].is_null()){
    const auto& pending=state_["pendingUpdate"];
    fields(pending,{"id","playlist","context"});string_field(pending,"id",64);
    require(pending.contains("playlist"),"storage_corrupt","Pending refresh has no playlist");
    validate_playlist(pending["playlist"], true);
    require(pending.value("context","")=="curated"||pending.value("context","")=="personal","storage_corrupt","Invalid pending refresh context");
  }
  if(state_.contains("updateOutcome")&&!state_["updateOutcome"].is_null()){
    const auto& outcome=state_["updateOutcome"];
    fields(outcome,{"id","outcome","contentBlocked"});string_field(outcome,"id",64);
    require(outcome.value("outcome","")=="deferred"||outcome.value("outcome","")=="applied"||outcome.value("outcome","")=="failed","storage_corrupt","Invalid playlist refresh outcome");
    require(outcome.contains("contentBlocked")&&outcome["contentBlocked"].is_boolean(),"storage_corrupt","Invalid playlist refresh policy result");
  }
  // Schema-v1 settings are optional overrides. Materialize known defaults in
  // snapshots without resetting any saved value or requiring an eager write.
  auto settings = expected["settings"]; settings.update(state_["settings"]); state_["settings"] = std::move(settings);
  global_policy_ = store_.load_global_policy(state_);
  if (!state_["stopped"].get<bool>() && !state_["sleeping"].get<bool>()) select();
}
void Playback::validate_state() const {
  require(state_["context"]=="curated"||state_["context"]=="personal","storage_corrupt","Invalid stored content context");
  require(state_["loop"]=="none"||state_["loop"]=="one"||state_["loop"]=="playlist","storage_corrupt","Invalid stored loop mode");
  require(state_["index"].is_number_integer()&&state_["index"]>=0&&state_["index"]<=4095,"storage_corrupt","Invalid stored index");
  const size_t count=state_["playlist"].is_null()?0:state_["playlist"]["items"].size();
  require(count?state_["index"].get<size_t>()<count:state_["index"]==0,"storage_corrupt","Stored index is outside the playlist");
  require(state_["order"].size()==count,"storage_corrupt","Stored shuffle order does not match playlist");
  std::vector<bool> seen(count,false);
  for(const auto& index:state_["order"]){
    require(index.is_number_integer()&&index>=0&&index<count,"storage_corrupt","Invalid stored shuffle index");
    auto i=index.get<size_t>();require(!seen[i],"storage_corrupt","Repeated stored shuffle index");seen[i]=true;
  }
  if(state_.contains("playlistUrl"))require(state_["playlistUrl"].is_string()&&state_["playlistUrl"].get_ref<const std::string&>().size()<=16384,"storage_corrupt","Invalid stored playlist URL");
  const auto& policy=state_["policy"];
  fields(policy,{"version","showMatureContent","strictPersonal","blockUnratedCurated"});
  require(policy.contains("version")&&policy["version"].is_number_integer()&&policy["version"]==1,"storage_corrupt","Invalid stored content policy version");
  for(auto key:{"showMatureContent","strictPersonal","blockUnratedCurated"})boolean(policy,key);
  require(state_["history"].size()<=100&&state_["history"].dump().size()<=1024*1024,"storage_corrupt","Stored history exceeds bounds");
  std::vector<std::string> ids;
  for(const auto& record:state_["history"]){
    auto id=string_field(record,"recordId",64);
    require(std::find(ids.begin(),ids.end(),id)==ids.end(),"storage_corrupt","Duplicate history identity");ids.push_back(id);
    require(record.contains("playedAtMs")&&record["playedAtMs"].is_number_integer()&&record["playedAtMs"]>=0,"storage_corrupt","Invalid history timestamp");
    require(record.contains("item"),"storage_corrupt","History item is missing");
    validate_playlist({{"items",Json::array({record["item"]})}}, true);
    require(record.value("context","curated")=="curated"||record.value("context","curated")=="personal","storage_corrupt","Invalid history context");
    if(record.contains("defaults"))validate_layer(record["defaults"], true);
  }
  for(auto key:{"boot","schedule"}){
    const auto& entry=state_[key];if(entry.is_null())continue;
    fields(entry,{"playlist","context","atMs","clear","id","activationTimes","playlistUrl","index"});
    if(entry.contains("id"))string_field(entry,"id",64);
    require(entry.contains("playlist"),"storage_corrupt","Stored boot/schedule playlist is missing");validate_playlist(entry["playlist"], true);
    require(entry.value("context","curated")=="curated"||entry.value("context","curated")=="personal","storage_corrupt","Invalid boot/schedule context");
    if(std::string_view(key)=="schedule")require(entry.contains("atMs")&&entry["atMs"].is_number_integer()&&entry["atMs"]>0,"storage_corrupt","Invalid stored schedule time");
  }
  if(state_.contains("scheduleOutcome")&&!state_["scheduleOutcome"].is_null()){
    const auto& outcome=state_["scheduleOutcome"];
    fields(outcome,{"id","atMs","executedAtMs","instance","generation","outcome","error"});
    string_field(outcome,"id",64);string_field(outcome,"instance",64);
    for(auto key:{"atMs","executedAtMs","generation"})require(outcome.contains(key)&&outcome[key].is_number_integer()&&outcome[key]>=0,"storage_corrupt","Invalid scheduled outcome identity");
    require(outcome.value("outcome","")=="applied"||outcome.value("outcome","")=="failed","storage_corrupt","Invalid scheduled outcome");
    if(outcome["outcome"]=="failed"){
      require(outcome.contains("error"),"storage_corrupt","Missing scheduled error");fields(outcome["error"],{"code","message"});
      string_field(outcome["error"],"code",64);string_field(outcome["error"],"message",4096);
    }
  }
}
void Playback::persist(Json next) {
  try { store_.save(next); }
  catch(const Error& error) {
    storage_error_=error.what();storage_retry_at_=now_+5000;++revision_;
    throw;
  }
  state_=std::move(next);storage_error_.clear();storage_retry_at_=0;++revision_;
}
bool Playback::allowed(const Json& item, const Json& s) const {
  if (s["policy"].value("showMatureContent", false) || (s["context"] == "personal" && !s["policy"].value("strictPersonal", false))) return true;
  auto rating = item.value("contentRating", "");
  if (rating == "mature") return false;
  return rating == "general" || s["context"] == "personal" || !s["policy"].value("blockUnratedCurated", false);
}
void Playback::validate_playlist(const Json& playlist, bool restoring) const {
  require(playlist.is_object() && playlist.contains("items") && playlist["items"].is_array(), "playlist_invalid", "Playlist must contain items");
  require(playlist["items"].size() <= 4096, "playlist_invalid", "At most 4096 items are allowed");
  // Leave room for occurrence snapshots and durable history within 4 MiB.
  require(playlist.dump().size()<=1024*1024,"playlist_invalid","Playlist exceeds 1 MiB");
  for(auto key:{"id","title","curator","dpVersion","slug","created"})optional_text(playlist,key);
  if(playlist.contains("defaults"))validate_layer(playlist["defaults"], restoring);
  for (const auto& item : playlist["items"]) {
    require(item.is_object(), "playlist_invalid", "Every item must be an object");
    require(item.dump().size()<=65536,"playlist_invalid","Artwork item exceeds 64 KiB");
    optional_text(item,"mimeType",256);
    for(auto key:{"id","title","slug","license","ref","refHash"})optional_text(item,key);
    if (item.contains("ffCapsuleId")) {
      const auto digest = string_field(item, "ffCapsuleId", 64);
      require(digest.size() == 64 && digest.find_first_not_of("0123456789abcdef") == digest.npos,
              "playlist_invalid", "Invalid offline capsule digest");
    }
    auto source = string_field(item, "source", 16384);
    require(source.starts_with("https://") || source.starts_with("http://") || source.starts_with("data:") || source.starts_with("ipfs://") || source.starts_with("onchfs://") || (!local_artwork_name(source).empty() && native_media(item)), "playlist_invalid", "Unsupported artwork URL scheme");
    if (item.contains("contentRating")) require(item["contentRating"].is_string(), "playlist_invalid", "contentRating must be a string");
    if (item.contains("contentReasons")) {
      require(item["contentReasons"].is_array(), "playlist_invalid", "contentReasons must be an array");
      for (const auto& reason : item["contentReasons"]) require(reason.is_string(), "playlist_invalid", "contentReasons must contain strings");
    }
    if (item.contains("duration")) require(item["duration"].is_number() && item["duration"].get<double>() >= 0, "playlist_invalid", "duration must be nonnegative");
    if(item.contains("display"))validate_display(item["display"], restoring);
    if(item.contains("metadata"))validate_metadata(item["metadata"]);
    for (auto key : {"override", "inlineManifest", "resolvedManifest"}) if(item.contains(key))validate_layer(item[key], restoring);
  }
}
void Playback::refresh_display(Occurrence& occurrence) {
  occurrence.display = resolve_display(state_["settings"], occurrence.defaults, occurrence.item,
                                       occurrence.adjustment);
  ++occurrence.display_revision;
}
void Playback::release_input() {
  if (input_release_ != InputRelease::immediate) {
    input_release_ = InputRelease::pending;
  } else if (input_) {
    input_({{"kind", "releaseAll"}});
  }
}
void Playback::defer_input_release(bool defer) {
  const bool pending = input_release_ == InputRelease::pending;
  input_release_ = defer ? InputRelease::deferred : InputRelease::immediate;
  if (!defer && pending) release_input();
}
void Playback::clear_recovery() {
  recovery_attempt_=0;recovery_at_=0;recovery_error_.clear();recovery_occurrence_.clear();
}
void Playback::select(bool recovering,bool skipping_failure) {
  if(!recovering && !skipping_failure)exhausted_items_.clear();
  if(!recovering){clear_recovery();presentation_failure_=nullptr;}
  release_input();
  eos_pending_ = false;
  selected_.reset();
  media_status_ = nullptr;
  engine_status_ = packaged_engine_;
  ++revision_;
  if (thermal_hold_ || state_["playlist"].is_null() || state_["stopped"] == true || state_["sleeping"] == true) { presented_.reset(); return; }
  const auto& items = state_["playlist"]["items"];
  if (items.empty()) { presented_.reset(); return; }
  int index = state_["index"].get<int>();
  require(index >= 0 && index < static_cast<int>(items.size()), "storage_corrupt", "Selection is outside playlist");
  if (!allowed(items[index], state_) || held_slot_blocked(state_["playlist"], index, state_)) {
    if (waiting_for_refresh_activation()) { presented_.reset(); return; }
    auto found = std::find_if(items.begin(), items.end(), [&](const Json& item) { return allowed(item, state_); });
    if (found == items.end()) { presented_.reset(); return; }
    auto next = state_; next["index"] = index = static_cast<int>(found - items.begin()); persist(std::move(next));
  }
  auto defaults=state_["playlist"].value("defaults",Json::object());
  const auto& item = state_["playlist"]["items"][index];
  selected_ = Occurrence{uuid(), item, resolve_display(state_["settings"], defaults, item), index, generation_, "loading", "", false,
    defaults,state_["context"].get<std::string>(),state_["playlist"].value("title",""),state_["playlist"].value("curator",""),
    state_["playlist"],state_.value("playlistUrl", ""),cast_id_};
  if(!recovering)autoplay_hold_ = !global_policy_.at("autoplay").get<bool>() || !selected_->display.value("autoplay", true);
  selected_at_ = now_;
}
bool Playback::media_paused(const std::string& occurrence) const {
  // Decode the first frame while paused, then start only once the host has
  // actually exposed it. Setup may cover the incoming work for minutes.
  const bool waiting=!occurrence.empty()&&(!presented_||presented_->id!=occurrence);
  return state_["paused"] == true || autoplay_hold_ || waiting;
}
void Playback::replace(Json& next, const Json& p) {
  fields(p, {"playlist", "index", "context", "playlistUrl", "boot", "onlyIfNoPlaylist", "activationTimes"});
  for(auto key:{"boot","onlyIfNoPlaylist"})if(p.contains(key))boolean(p,key);
  require(p.contains("playlist"), "invalid_params", "Missing playlist");
  validate_playlist(p["playlist"]);
  optional_text(p,"playlistUrl",16384);
  next["cohortPlan"] = nullptr;
  next["playlist"] = p["playlist"];
  next["context"] = p.value("context", "curated");
  require(next["context"] == "curated" || next["context"] == "personal", "invalid_params", "Invalid content context");
  const auto& items = next["playlist"]["items"];
  if(p.contains("index")) require(p["index"].is_number_integer() && p["index"] >= 0 && p["index"] <= 4096, "invalid_params", "Invalid playlist index");
  int index = p.value("index", 0);
  require(index >= 0 && (items.empty() ? index == 0 : index < static_cast<int>(items.size())), "invalid_params", "Index out of bounds");
  bool any = items.empty();
  for (const auto& item : items) any |= allowed(item, next);
  require(any, "content_blocked", "No item is allowed by the content policy; current playback retained");
  if (!items.empty() && !allowed(items[index], next)) {
    for (size_t i = 0; i < items.size(); ++i) if (allowed(items[i], next)) { index = static_cast<int>(i); break; }
  }
  next["index"] = index;
  next["playlistUrl"] = p.value("playlistUrl", "");
  if(p.value("boot",false)){auto boot=p;boot.erase("boot");boot.erase("onlyIfNoPlaylist");next["boot"]=boot;}
  next["stopped"] = items.empty(); next["paused"] = false; next["schedule"] = nullptr;
  next["scheduleOutcome"] = nullptr;
  next["pendingUpdate"] = nullptr; next["updateOutcome"] = nullptr;
  // Each explicit cast starts a fresh playback mode, matching CanvasService.
  next["shuffle"] = false; next["loop"] = "playlist";
  std::vector<int> order(items.size()); std::iota(order.begin(), order.end(), 0);
  next["order"] = order;
}
// Returns 0 for a conditional no-op, 1 for a deferred plan, 2 for a selection.
int Playback::prepare_replace(Json& next,const Json& p){
    replace(next,p);
    // Conditional fallback is checked at the serialized commit boundary, not
    // before controld performs a network fetch that can race a newer cast.
    if(p.value("onlyIfNoPlaylist",false)&&!state_["playlist"].is_null()&&!state_["playlist"]["items"].empty())
      return 0;
    bool reselection = true;
    if(p.contains("activationTimes")){
      Json plan={{"playlist",p["playlist"]},{"context",p.value("context","curated")},{"playlistUrl",p.value("playlistUrl","")},{"activationTimes",p["activationTimes"]},{"started",true}};
      validate_plan(plan);Json active;
      auto wall=clock_started_?wall_now_:std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::system_clock::now().time_since_epoch()).count();
      auto list=cohort(plan,wall,active);plan["activeAt"]=active;
      if(list["items"].empty()){
        next=state_;reselection=false;
        // Keep the current artwork until activation, but retire superseded
        // pending owners now. They must not erase this newly accepted plan.
        for(auto key:{"schedule","scheduleOutcome","pendingUpdate","updateOutcome"})next[key]=nullptr;
      }
      else {
        size_t index=0;
        // Preserve an eligible requested occurrence after cohort filtering.
        // A future start item cannot bypass its displayAt boundary.
        if(p.contains("index")) {
          const auto& requested=p["playlist"]["items"][p["index"].get<size_t>()];
          for(size_t i=0;i<list["items"].size();++i)if(list["items"][i]==requested){index=i;break;}
        }
        replace(next,{{"playlist",list},{"index",index},{"context",plan["context"]},{"playlistUrl",plan["playlistUrl"]}});
      }
      next["cohortPlan"]=plan;
      if(p.value("boot",false)) { next["boot"]={{"playlist",p["playlist"]},{"context",plan["context"]},{"activationTimes",plan["activationTimes"]},{"playlistUrl",plan["playlistUrl"]}}; if(p.contains("index"))next["boot"]["index"]=p["index"]; }
    }
 return reselection?2:1;
}
void Playback::import_legacy(const Json& records) {
  // This runs offline, before promotion. Use the same policy and playlist
  // validation as a live cast. Never import a renderer's ready/presented bit.
  for (auto key : {"contentPolicy", "castInfo", "boot_playlist"})
    require(records.contains(key), "invalid_params", "Incomplete legacy storage export");
  if (!records["contentPolicy"].is_null()) command("policy.set", records["contentPolicy"]);
  Json settings = Json::object();
  auto display = records.value("displaySettings", Json(nullptr));
  for (auto key : {"scaling", "framing", "tombstone"})
    if (display.contains(key)) settings[key] = display[key];
  if (records.contains("defaultItemDuration")) settings["durationSeconds"] = records["defaultItemDuration"];
  if (!settings.empty()) command("settings.set", settings);
  const auto& boot = records["boot_playlist"];
  if (!boot.is_null()) {
    bool wrapped = boot.contains("playlist");
    command("boot.set", {{"playlist", wrapped ? boot.at("playlist") : boot},
                         {"context", wrapped ? boot.value("contentContext", "curated") : "curated"}});
  }
  const auto& cast = records["castInfo"];
  require(cast.is_null() || cast.is_object(), "invalid_params", "Invalid legacy cast record");
  if (cast.is_null() || !cast.contains("playlist") || cast["playlist"].is_null()) {
    if (!boot.is_null()) command("playback.replace", state_["boot"]);
  } else {
    command("playback.replace", {{"playlist", cast["playlist"]}, {"index", cast.value("index", 0)},
                               {"context", cast.value("contentContext", "curated")},
                               {"playlistUrl", cast.value("playlistUrl", "")}});
    command("playback.mode", {{"loop", cast.value("loopMode", "playlist")}});
    if (cast.value("shuffle", false)) {
      // The web player already persisted the shuffled sequence in items.
      // Keep replace()'s sequential indices and resume from the saved slot.
      auto next = state_;
      next["shuffle"] = true;
      persist(std::move(next));
    }
    if (cast.value("isPaused", false)) command("playback.pause", {{"paused", true}});
  }
  auto schedule = records.value("dp1_scheduled_tasks", Json(nullptr));
  if (!schedule.is_null()) {
    command("schedule.set", {{"playlist", schedule.at("dp1CallData")}, {"atMs", schedule.at("atMs")},
                             {"context", schedule.value("contentContext", "curated")}});
  }
}

void Playback::system_boot(const std::string& id){
  require(!id.empty()&&id.size()<=64,"invalid_params","Invalid operating-system boot ID");
  if(state_.value("systemBootId","")==id)return;
  auto next=state_;int action=0;
  // First native activation imports current intent; only a later OS boot
  // executes the saved boot playlist. A host restart keeps the same boot ID.
  if(state_.contains("systemBootId")&&!state_["boot"].is_null()){
    auto params=state_["boot"];params.erase("clear");params.erase("id");params.erase("atMs");
    // Older refreshes could leave the saved index beyond the new playlist.
    const bool stale_index = params.contains("index") && params["index"] > 0 &&
                             params["index"] >= params["playlist"]["items"].size();
    if (stale_index) params["index"] = 0;
    try{action=prepare_replace(next,params);}
    catch(const Error& error){
      if(error.code!="content_blocked")throw;
      next=state_;next["scheduleOutcome"]={{"id",uuid()},{"atMs",0},{"executedAtMs",wall_ms()},{"instance",instance_},{"generation",generation_},{"outcome","failed"},{"error",{{"code",error.code},{"message",error.what()}}}};
    }
    if (stale_index) next["boot"]["index"] = 0;
  }
  // The retained frame still owns refreshes when boot restores the same deferred source.
  bool same_hold = false;
  if (action == 1 && waiting_for_refresh_activation()) {
    auto& restored = next["cohortPlan"];
    restored["started"] = false;
    same_hold = restored == state_["cohortPlan"];
    restored["started"] = !same_hold;
  }
  next["systemBootId"]=id;persist(std::move(next));
  if(action){++generation_;if(!same_hold)cast_id_=uuid();if(action==2)select();}
}

Json Playback::command(std::string_view method, const Json& p) {
  require(p.is_object(), "invalid_params", "params must be an object");
  if (method == "state.get" || method == "health.get") { fields(p, {}); return snapshot(); }
  if (method == "history.list") { fields(p, {}); return {{"records", state_["history"]}, {"incomplete", state_["historyIncomplete"]}, {"activeOccurrence", presented_ ? Json(presented_->id) : Json(nullptr)}}; }
  if (method == "history.resolve") {
    fields(p, {"recordId"}); auto id = string_field(p, "recordId", 64);
    for (const auto& record : state_["history"]) if (record["recordId"] == id) return record;
    throw Error("not_found", "History record is no longer retained");
  }
  if(method=="health.set"){
    fields(p,{"thermal","serviceFailure"});require(!p.empty(),"invalid_params","Missing health signal");
    bool thermal=p.contains("thermal")?boolean(p,"thermal"):thermal_hold_;
    bool service=p.contains("serviceFailure")?boolean(p,"serviceFailure"):service_failure_;
    if (thermal != thermal_hold_ || service != service_failure_) {
      const bool thermal_changed = thermal != thermal_hold_;
      thermal_hold_ = thermal;
      service_failure_ = service;
      if (thermal_changed) select();
      ++revision_;
    }
    return {{"outcome","applied"},{"generation",generation_},{"revision",revision_}};
  }
  if (method == "artworkPermissions.get") { fields(p, {}); return global_policy_; }
  if (method == "policy.get") { fields(p, {}); return state_["policy"]; }
  if (method == "artworkPermissions.set") {
    validate_global_policy(p);
    auto next = global_policy_;
    next.update(p, true);
    auto grants = next;
    grants["cache"] = global_policy_.at("cache");
    bool changed = grants != global_policy_;
    bool cache_changed = next.at("cache") != global_policy_.at("cache");
    const bool durable = store_.save_global_policy(next);
    global_policy_ = std::move(next);
    if (changed) {
      presented_.reset();
      select();
    }
    if (cache_changed && !changed) ++revision_;
    // Once rename succeeds, even a directory-sync error must retire old grants.
    require(durable, "storage_unavailable", "Global policy applied but directory sync failed; retry the update");
    return {{"outcome", changed && selected_ ? "accepted" : "applied"}, {"generation", generation_},
            {"revision", revision_}, {"occurrence", selected_ ? Json(selected_->id) : Json(nullptr)}};
  }
  if (method == "playback.update") {
    if(!p.contains("activationTimes"))return update_playlist(p);
    fields(p,{"items","context","retireBlockedCurrent","activationTimes"});
    // A live source may publish its first displayAt after the initial cast.
    // Stopping playback revokes that ownership even though the playlist remains on disk.
    const bool timed = state_.contains("cohortPlan") && !state_["cohortPlan"].is_null();
    require(timed || (!state_["playlist"].is_null() && state_["stopped"] == false),
            "stale_generation", "Timed source no longer owns playback");
    const auto& previous = timed ? state_["cohortPlan"] : state_;
    Json plan = {{"playlist", previous["playlist"]}, {"context", previous["context"]},
                 {"playlistUrl", previous["playlistUrl"]}, {"started", true}};
    require(p.contains("items"),"invalid_params","Missing items");
    if (plan["playlist"]["items"] != p["items"]) {
      plan["playlist"].erase("signature");
      plan["playlist"].erase("signatures");
    }
    if (p.contains("retireBlockedCurrent")) boolean(p,"retireBlockedCurrent");
    require(previous["context"]!="curated" || p.value("context","curated")!="personal",
            "invalid_params","Refresh cannot reclassify curated playback as personal");
    plan["playlist"]["items"]=p["items"];plan["activationTimes"]=p["activationTimes"];
    plan["context"]=p.value("context",plan["context"].get<std::string>());validate_plan(plan);validate_playlist(plan["playlist"]);
    Json active;auto list=cohort(plan,clock_started_?wall_now_:wall_ms(),active);plan["activeAt"]=active;
    if(list["items"].empty()){
      plan["started"] = false;
      auto next=state_;
      const auto& items = plan["playlist"]["items"];
      auto policy = state_;
      policy["context"] = plan["context"];
      const bool retain_frame = presented_ && presented_->cast_id == cast_id_;
      const bool restore_frame = retain_frame && (!selected_ || selected_->id != presented_->id);
      const bool retire=!retain_frame || p.value("retireBlockedCurrent",false) || items.empty() ||
                        refresh_blocks(*presented_,items,policy);
      const auto update_id=uuid();
      next["updateOutcome"]={{"id",update_id},{"outcome","applied"},{"contentBlocked",false}};
      refresh_boot(next, previous["playlist"], plan["playlist"], plan["context"], plan);
      next["pendingUpdate"]=nullptr;
      if (retire) {
        next["playlist"]=list;
        next["context"]=plan["context"];
        next["playlistUrl"]=plan["playlistUrl"];
        update_order(next,0);
        next["stopped"]=true;
      } else if (restore_frame) {
        // A refresh can arrive after the next slot starts loading but before it
        // reaches the screen. Only the frame already on screen may finish.
        next["playlist"] = presented_->playlist;
        next["context"] = presented_->context;
        next["playlistUrl"] = presented_->playlist_url;
        update_order(next, presented_->index);
      }
      next["cohortPlan"]=plan;
      persist(std::move(next));++generation_;
      if (retire) select();
      else if (restore_frame) {
        clear_recovery();
        eos_pending_ = false;
        presentation_failure_ = media_status_ = nullptr;
        engine_status_ = packaged_engine_;
        selected_ = presented_;
      }
      return {{"outcome","applied"},{"generation",generation_},{"revision",revision_},{"updateId",update_id}};
    }
    auto params=p;params.erase("activationTimes");params["items"]=list["items"];
    return update_playlist(params,plan);
  }
  if (method == "connectivity.set") {
    fields(p,{"online", "sources"});
    const bool online = boolean(p, "online");
    bool sources_changed = false;
    if (p.contains("sources")) {
      const auto& sources = p.at("sources");
      fields(sources, {"bundleRoot", "gatewayHosts", "gateway", "userAgent"});
      require(string_field(sources, "bundleRoot", 4096).starts_with("/"), "invalid_params", "Expected an absolute bundle directory");
      optional_text(sources, "gateway", 4096);
      optional_text(sources, "userAgent", 1024);
      if (sources.contains("gatewayHosts")) {
        require(sources["gatewayHosts"].is_array() && sources["gatewayHosts"].size() <= 1024, "invalid_params", "Invalid gateway host list");
        for (const auto& host : sources["gatewayHosts"]) require(host.is_string() && host.get_ref<const std::string&>().size() <= 253, "invalid_params", "Invalid gateway host");
      }
      sources_changed = connectivity_known_ && sources != artwork_sources_;
      artwork_sources_ = sources;
    }
    bool was_online=online_;online_=online;connectivity_known_=true;
    // An offline edge replaces the browser network namespace. Keep the old
    // presentation until the isolated replacement has a frame. An online edge
    // only retries failed/incomplete replay; healthy works keep their state.
    bool incomplete_offline=!media_status_.is_null()&&media_status_.value("offline",false)&&(media_status_.value("cacheMisses",uint64_t{0})>0||media_status_.value("unsupportedWorkerTargets",uint64_t{0})>0);
    if(selected_&&(sources_changed||(was_online&&!online_)||(!was_online&&online_&&(selected_->stage=="failed"||incomplete_offline)))){clear_recovery();select(true);}
    ++revision_;return {{"outcome","applied"}};
  }
  if (method == "display.rejected") {
    fields(p, {});
    rejection_until_ = now_ + 8000;
    rejection_generation_ = generation_;
    ++revision_;
    return {{"outcome", "applied"}};
  }
  if (method == "overlay.set") {
    validate_overlay(p);
    auto owner=p["owner"].get<std::string>(),state=p["state"].get<std::string>();
    // Legacy ready is a non-visible setup state, not a success title panel.
    if(state=="hidden" || (owner=="setup" && state=="ready")) {
      if(!overlay_.is_null() && overlay_["owner"]==owner)overlay_=nullptr;
    } else { overlay_=p; if (!overlay().is_null()) release_input(); }
    ++revision_; return {{"outcome", "applied"}};
  }
  if (method == "input.dispatch") {
    require(p.contains("occurrence") && presented_ && p["occurrence"] == presented_->id, "stale_occurrence", "Input target is not currently presented");
    require(overlay().is_null() && state_["sleeping"] == false && (!selected_ || selected_->stage!="failed") && !composition_pending(), "input_blocked", "Device UI or a pending presentation change owns focus");
    require(static_cast<bool>(input_), "unavailable", "No presentation backend");
    auto kind=string_field(p,"kind",32);
    if(kind=="move"){
      fields(p,{"kind","occurrence","x","y","dx","dy","referenceWidth"});
      if(p.contains("referenceWidth"))require(p["referenceWidth"].is_number()&&p["referenceWidth"]>=1&&p["referenceWidth"]<=16384&&!p.contains("x")&&!p.contains("y"),"invalid_params","Reference width requires relative coordinates and must be 1..16384");
      for(auto key:{"x","y","dx","dy"})if(p.contains(key))require(p[key].is_number()&&std::abs(p[key].get<double>())<=16384,"invalid_params","Input coordinate exceeds bounds");
    }else if(kind=="button"){
      fields(p,{"kind","occurrence","button","down","clicks"});boolean(p,"down");
      for(auto key:{"button","clicks"})if(p.contains(key))require(p[key].is_number_integer()&&p[key]>=1&&p[key]<=3,"invalid_params","Invalid mouse button/click count");
    }else if(kind=="wheel"){
      fields(p,{"kind","occurrence","dx","dy"});
      for(auto key:{"dx","dy"})if(p.contains(key))require(p[key].is_number_integer()&&p[key]>=-32768&&p[key]<=32768,"invalid_params","Invalid scroll amount");
    }else if(kind=="key"){
      fields(p,{"kind","occurrence","usage","down"});boolean(p,"down");
      require(p.contains("usage")&&p["usage"].is_number_integer()&&p["usage"]>=1&&p["usage"]<=255,"invalid_params","Invalid USB HID keyboard usage");
      physical_key(p["usage"].get<int>());
    }else if(kind=="text"){fields(p,{"kind","occurrence","text"});string_field(p,"text",4096);}
    else if(kind=="releaseAll")fields(p,{"kind","occurrence"});
    else throw Error("invalid_params","Unknown input kind");
    input_(p); return {{"outcome", "accepted"}};
  }
  if (method == "media.seek") {
    fields(p,{"occurrence","seconds"});
    require(presented_ && p.value("occurrence","") == presented_->id,"stale_occurrence","Seek target is not currently presented");
    require(p.contains("seconds") && p["seconds"].is_number(),"invalid_params","Expected seconds");
    auto seconds=p["seconds"].get<double>();require(std::isfinite(seconds)&&seconds>=0&&seconds<=86400000,"invalid_params","Invalid seek position");
    require(bool(seek_),"unavailable","No media backend");seek_(presented_->id,seconds);
    return {{"outcome","accepted"},{"occurrence",presented_->id}};
  }
  if(method=="presentation.set"){
    fields(p,{"occurrence","display"});
    auto id=string_field(p,"occurrence",64);
    require(selected_&&presented_&&selected_->id==id&&presented_->id==id&&selected_->stage=="presented","stale_occurrence","Presentation target changed or is still loading");
    require(!transition_stage_ || composition_.value("occurrence", "") != id,
            "stale_occurrence", "Presentation is still transitioning");
    require(p.contains("display"),"invalid_params","Missing presentation settings");
    auto adjustment=p["display"];fields(adjustment,{"scaling","margin","background"});validate_display(adjustment);
    for(const auto& field:adjustment.items())require(allows_display_override(selected_->display,field.key()),"presentation_locked","Artwork locks "+field.key());
    auto colors=adjustment;colors.erase("margin");validate_settings(colors);
    selected_->adjustment.update(adjustment);
    refresh_display(*selected_);
    presented_->adjustment=selected_->adjustment;presented_->display=selected_->display;presented_->display_revision=selected_->display_revision;
    ++revision_;
    return {{"outcome","accepted"},{"generation",generation_},{"revision",revision_},{"occurrence",id},{"acceptedCompositionRevision",composition_revision_}};
  }
  auto next = state_;
  bool reselection = false, generation = false;
  if (method == "playback.replace") {
    const auto action=prepare_replace(next,p);
    if(!action)return {{"outcome","applied"},{"generation",generation_},{"revision",revision_},{"occurrence",selected_?Json(selected_->id):Json(nullptr)}};
    generation=true;reselection=action==2;
  }
  else if (method == "playback.select") {
    fields(p, {"index"}); require(!next["playlist"].is_null() && p.contains("index") && p["index"].is_number_integer(), "invalid_params", "Missing playlist or index");
    require(p["index"] >= 0 && p["index"] <= 4096, "invalid_params", "Invalid playlist index");
    int index = p["index"].get<int>();
    require(index >= 0 && index < static_cast<int>(next["playlist"]["items"].size()), "invalid_params", "Index out of bounds");
    if (waiting_for_refresh_activation() && index != next["index"]) {
      require(apply_pending_update(index), "invalid_params", "Selected work is outside the eligible cohort");
      return {{"outcome",selected_ ? "accepted" : "applied"},{"generation",generation_},{"revision",revision_},
              {"occurrence",selected_ ? Json(selected_->id) : Json(nullptr)}};
    }
    require(allowed(next["playlist"]["items"][index], next) && !held_slot_blocked(next["playlist"], index, next),
            "content_blocked", "Selected work is blocked");
    next["index"] = index; next["stopped"] = false; reselection = true;
  } else if (method == "playback.refresh") { fields(p, {}); require(selected_.has_value(), "no_artwork", "No active artwork to refresh"); reselection = true; }
  else if (method == "playback.stop" || method == "playback.cancel") { fields(p, {}); next["stopped"] = true; next["cohortPlan"]=nullptr; next["schedule"]=nullptr; next["pendingUpdate"]=nullptr; next["updateOutcome"]=nullptr; reselection = generation = true; }
  else if (method == "playback.sleep") { fields(p, {"sleeping"}); next["sleeping"] = boolean(p, "sleeping"); reselection = next["sleeping"] != state_["sleeping"]; }
  else if (method == "playback.pause") { fields(p, {"paused"}); next["paused"] = boolean(p, "paused"); }
  else if (method == "playback.mode") {
    fields(p, {"loop", "shuffle"});
    if (p.contains("loop")) { require(p["loop"] == "none" || p["loop"] == "playlist" || p["loop"] == "one", "invalid_params", "Invalid loop mode"); next["loop"] = p["loop"]; }
    if (p.contains("shuffle")) {
      next["shuffle"] = boolean(p, "shuffle");
      require(!next["playlist"].is_null() && !next["playlist"]["items"].empty(), "invalid_params", "No playlist to shuffle");
      std::vector<int> order(next["playlist"]["items"].size());
      std::iota(order.begin(),order.end(),0);
      if (next["shuffle"] == true) {
        // Anchor the current slot, not its URL: adjacent equal-URL works are
        // distinct occurrences and every remaining slot must still be visited.
        auto current=std::find(order.begin(),order.end(),next["index"].get<int>());
        std::iter_swap(order.begin(),current);
        std::mt19937 random(std::random_device{}());
        std::shuffle(order.begin()+1,order.end(),random);
      }
      next["order"]=order;
    }
  } else if (method == "settings.set") { validate_settings(p); next["settings"].update(p); }
  else if (method == "policy.set") {
    fields(p, {"version", "showMatureContent", "strictPersonal", "blockUnratedCurated"});
    require(p.contains("version")&&p["version"].is_number_integer()&&p["version"] == 1, "invalid_params", "Unsupported content policy version");
    next["policy"] = {{"version", 1}, {"showMatureContent", boolean(p, "showMatureContent")}, {"strictPersonal", boolean(p, "strictPersonal")}, {"blockUnratedCurated", boolean(p, "blockUnratedCurated")}};
    reselection = !selected_ || !allowed(selected_->item, next) ||
                  (selected_->cast_id == cast_id_ && held_slot_blocked(selected_->playlist, selected_->index, next));
  } else if (method == "schedule.set" || method == "boot.set") {
    fields(p, {"playlist", "context", "atMs", "clear", "index", "playlistUrl", "activationTimes"});
    auto key = method == "boot.set" ? "boot" : "schedule";
    if (p.value("clear", false)) next[key] = nullptr;
    else {
      require(p.contains("playlist"), "invalid_params", "Missing playlist"); validate_playlist(p["playlist"]);
      require(p.value("context","curated")=="curated"||p.value("context","curated")=="personal","invalid_params","Invalid content context");
      if (method == "schedule.set") require(p.contains("atMs") && p["atMs"].is_number_integer() && p["atMs"].get<int64_t>() > 0, "invalid_params", "Invalid schedule timestamp");
      optional_text(p,"playlistUrl",16384);
      if(p.contains("index"))require(p["index"].is_number_integer()&&p["index"]>=0&&(p["playlist"]["items"].empty()?p["index"]==0:p["index"]<p["playlist"]["items"].size()),"invalid_params","Invalid scheduled index");
      if(p.contains("activationTimes")){auto plan=p;plan.erase("atMs");plan.erase("clear");plan.erase("index");validate_plan(plan);}
      next[key] = p;
      if(method=="schedule.set")next[key]["id"]=uuid();
    }
    if(method=="schedule.set")next["scheduleOutcome"]=nullptr;
  } else throw Error("unknown_method", "Method is not supported by this runtime");
  const bool was_paused = state_["paused"].get<bool>();
  persist(std::move(next));
  if(method=="policy.set"&&presented_){
    // The outgoing occurrence can belong to a different playlist/context
    // while a new cast loads. Policy retirement must inspect that occurrence,
    // not borrow the incoming cast's potentially more permissive context.
    auto presented_policy=state_;presented_policy["context"]=presented_->context;
    if(!allowed(presented_->item,presented_policy) ||
       (presented_->cast_id == cast_id_ && held_slot_blocked(presented_->playlist, presented_->index, state_))){release_input();presented_.reset();}
  }
  if (method == "playback.pause") autoplay_hold_ = false;
  if (method == "settings.set") {
    if (selected_) refresh_display(*selected_);
    if (presented_) refresh_display(*presented_);
  }
  if (generation) { ++generation_; cast_id_=uuid(); }
  if (reselection) select();
  if (state_["paused"] == true && !was_paused) pause_at_ = now_;
  if (state_["paused"] == false && was_paused) presented_at_ += now_ - pause_at_;
  return {{"outcome", reselection && selected_ ? "accepted" : "applied"}, {"generation", generation_}, {"revision", revision_}, {"occurrence", selected_ ? Json(selected_->id) : Json(nullptr)}};
}
Json Playback::overlay() const {
  // A reconnect advisory can yield before presentation. Waiting for presented()
  // here would deadlock because the overlay itself blocks presentation.
  if (overlay_.is_object() && overlay_.value("owner", "") == "setup" &&
      (overlay_.value("state", "") == "connecting" ||
       (overlay_.contains("fields") && overlay_["fields"].value("passive",false))) && connectivity_known_ && !online_ &&
      (presented_ || (selected_ && selected_->frame_available && selected_->stage != "failed")))
    return nullptr;
  return overlay_;
}

Json Playback::snapshot() const {
  Json waiting_source = nullptr;
  if (!selected_ && !presented_ && state_.contains("cohortPlan") && !state_["cohortPlan"].is_null()) {
    const auto& plan = state_["cohortPlan"];
    waiting_source = {{"playlist",plan["playlist"]},{"playlistUrl",plan["playlistUrl"]},{"context",plan["context"]}};
  }
  return {{"waitingSource",std::move(waiting_source)},{"instance", instance_}, {"generation", generation_}, {"revision", revision_}, {"protection",{{"thermal",thermal_hold_},{"serviceFailure",service_failure_}}},
          {"host", "responsive"}, {"engine",engine_status_}, {"notice",runtime_notice()}, {"renderer", selected_ ? selected_->stage : "absent"},
          {"storage", {{"health",storage_error_.empty()?"available":"write_failed"},{"error",storage_error_}}},
          {"presentationFailure",presentation_failure_},
          {"composition",presented_&&!composition_.is_null()&&composition_.value("occurrence","")==presented_->id?composition_:Json(nullptr)},
          {"recovery",{{"attempt",recovery_attempt_},{"limit",3},{"pending",recovery_at_!=0},
            {"exhausted",selected_&&selected_->stage=="failed"&&recovery_attempt_==3},
            {"lastError",recovery_error_},{"failedOccurrence",recovery_occurrence_},
            {"retryDelayMs",recovery_at_?1000<<recovery_attempt_:0}}},
          {"selected", occurrence_json(selected_)}, {"presented", occurrence_json(presented_)},
          {"presentedOwnsCast", presented_ && presented_->cast_id == cast_id_},
          {"presentedPlaylist", presented_ ? presented_->playlist : Json(nullptr)},
          {"presentedPlaylistUrl", presented_ ? presented_->playlist_url : ""},
          {"stopped", state_["stopped"]}, {"sleeping", state_["sleeping"]}, {"paused", state_["paused"]},
          {"playlist", state_["playlist"]}, {"playlistUrl", state_.value("playlistUrl", "")},
          {"artworkPermissions", global_policy_},
          {"loop", state_["loop"]}, {"shuffle", state_["shuffle"]}, {"settings", state_["settings"]},
          {"context", state_["context"]}, {"overlay", overlay()}, {"online", online_}, {"connectivityKnown",connectivity_known_}, {"media",media_status_}, {"mediaPaused",media_paused()},
          {"schedule",state_["schedule"]},{"scheduleOutcome",state_.value("scheduleOutcome",Json(nullptr))},
          {"pendingUpdate",state_.value("pendingUpdate",Json(nullptr)).is_null()?Json(nullptr):Json{
            {"id",state_["pendingUpdate"]["id"]},{"itemCount",state_["pendingUpdate"]["playlist"]["items"].size()}}},
          {"updateOutcome",state_.value("updateOutcome",Json(nullptr))}};
}
void Playback::runtime_notice(const std::string& id,const std::string& message){
 if(!selected_||selected_->id!=id)return;
 Json notice={{"occurrence",id},{"category","offline_dependencies"},{"message",message.substr(0,1024)}};
 if(notice!=runtime_notice_){runtime_notice_=std::move(notice);++revision_;}
}
void Playback::media_status(const std::string& id,const Json& status) {
  if(!selected_||selected_->id!=id||now_-media_status_at_<1000)return;
  media_status_=status;media_status_["occurrence"]=id;media_status_["sampledAtMs"]=wall_ms();
  ++revision_;media_status_at_=now_;
}
void Playback::committed(const std::string& id) {
  if (!selected_ || selected_->id != id || selected_->stage == "failed") return;
  if (selected_->navigation_committed) return;
  selected_->navigation_committed = true; selected_->stage = "committed"; ++revision_;
}
void Playback::frame_ready(const std::string& id) {
  if(selected_&&selected_->id==id&&selected_->stage=="committed")selected_->frame_available=true;
}
void Playback::composed(const std::string& id, uint64_t display_revision,
                        const Json* transition_stage) {
  // Only the host calls this, after submitting a frame with this configuration.
  // An old buffer/configuration acknowledgement cannot confirm a later write.
  if(!presented_||presented_->id!=id||presented_->display_revision!=display_revision)return;
  const auto& display = transition_stage ? *transition_stage : presented_->display;
  if (!composition_.is_null() && composition_.value("occurrence", "") == id &&
      composed_display_revision_ == display_revision && transition_stage_ == bool(transition_stage))
    return;
  transition_stage_ = bool(transition_stage);
  composed_display_revision_=display_revision;++composition_revision_;++revision_;
  composition_={{"occurrence",id},{"revision",composition_revision_},{"displayRevision",display_revision},{"display",display}};
}
void Playback::presented(const std::string& id) {
  if (!selected_ || selected_->id != id || !selected_->navigation_committed || selected_->stage == "failed" || (presented_ && presented_->id == id)) return;
  selected_->stage = "presented"; presented_ = selected_; presented_at_ = now_; ++revision_;
  presentation_failure_=nullptr;
  if(state_["paused"]==true)pause_at_=now_;
  auto next = state_;
  Json record = {{"recordId", id}, {"playedAtMs", wall_ms()}, {"item", selected_->item}, {"context", selected_->context}, {"defaults", selected_->defaults}};
  record["index"] = selected_->index;
  record["castId"] = selected_->cast_id;
  next["history"].insert(next["history"].begin(), record);
  while (next["history"].size() > 100 || next["history"].dump().size()>1024*1024) next["history"].erase(next["history"].end() - 1);
  try { persist(std::move(next)); } catch (const Error&) { state_["historyIncomplete"] = true; }
}
void Playback::failed(const std::string& id, std::string reason) {
  if(presented_&&presented_->id==id&&(!selected_||selected_->id!=id)){
    // The outgoing frame has its own lifecycle while its replacement loads.
    // Retire it without retrying over, or charging the budget of, that newer
    // selection. A frozen texture is not a healthy presented occurrence.
    release_input();
    presentation_failure_={{"occurrence",id},{"generation",presented_->generation},{"error",reason}};
    presented_.reset();++revision_;return;
  }
  if (!selected_ || selected_->id != id || selected_->stage == "failed") return;
  release_input(); selected_->stage = "failed"; selected_->error = std::move(reason);
  recovery_error_=selected_->error;recovery_occurrence_=id;
  recovery_at_=recovery_attempt_<3?now_+(1000<<recovery_attempt_):0;
  if(!media_status_.is_null())media_status_["workerResponsive"]=false;
  if (presented_ && presented_->id == id) presented_.reset();
  ++revision_;
}
double Playback::duration() const {
  if (!selected_) return 0;
  if (selected_->display.value("loop", true) && allows_display_override(selected_->display,"duration") && state_["settings"].contains("durationSeconds") && !state_["settings"]["durationSeconds"].is_null())
    return state_["settings"]["durationSeconds"].get<double>();
  return selected_->item.value("duration", 0.0);
}
bool Playback::waiting_for_refresh_activation() const {
  const auto plan = state_.find("cohortPlan");
  return plan != state_.end() && !plan->is_null() && !plan->value("started", true);
}
void Playback::advance(bool skipping_failure) {
  // The old list only supplies the outgoing frame. Its next slot may itself
  // be a work the refreshed source has scheduled for later.
  if (!selected_ || state_["paused"] == true) return;
  if(apply_pending_update() || waiting_for_refresh_activation())return;
  auto next = state_; const auto& items = next["playlist"]["items"];
  auto order = next["order"].get<std::vector<int>>();
  if (order.size() != items.size()) { order.resize(items.size()); std::iota(order.begin(), order.end(), 0); }
  if (state_["shuffle"] == false) std::iota(order.begin(), order.end(), 0);
  auto it = std::find(order.begin(), order.end(), selected_->index);
  size_t pos = it == order.end() ? 0 : static_cast<size_t>(it - order.begin());
  for (size_t n = 0; n < order.size(); ++n) {
    if (skipping_failure || state_["loop"] != "one") {
      if (pos + 1 == order.size() && state_["loop"] == "none") return;
      pos = (pos + 1) % order.size();
    }
    if(skipping_failure && exhausted_items_.contains(order[pos]))continue;
    if (allowed(items[order[pos]], next)) { next["index"] = order[pos]; persist(std::move(next)); select(false,skipping_failure); return; }
  }
}
void Playback::ended(const std::string& id) {
  // Keep a completion that raced initial pausing; tick only consumes it after
  // presentation. A retired occurrence can never advance the replacement.
  if (selected_ && selected_->id == id && (selected_->stage=="committed"||selected_->stage=="presented") && selected_->display.value("loop", true) == false) eos_pending_ = true;
}
void Playback::execute_schedule(int64_t wall_time_ms){
  const auto task=state_["schedule"];
  auto next=state_;
  Json outcome={{"id",task.value("id",uuid())},{"atMs",task["atMs"]},{"executedAtMs",wall_time_ms},
    {"instance",instance_},{"generation",generation_},{"outcome","applied"}};
  int action=0;
  try{
    auto params=task;params.erase("atMs");params.erase("clear");params.erase("id");
    action=prepare_replace(next,params);next["schedule"]=nullptr;outcome["generation"]=generation_+1;
  }catch(const Error& error){
    // A policy change can invalidate an already scheduled playlist. Retire
    // the task atomically, retain current playback, and expose the failure.
    // Leaving a permanently due task would crash/retry on every host restart.
    next=state_;next["schedule"]=nullptr;outcome["outcome"]="failed";
    outcome["error"]={{"code",error.code},{"message",error.what()}};
  }
  next["scheduleOutcome"]=outcome;persist(std::move(next));
  if(action){++generation_;cast_id_=uuid();if(action==2)select();}
}
void Playback::tick(int64_t monotonic_ms, int64_t wall_time_ms) {
  wall_now_=wall_time_ms;
  if (!clock_started_) { selected_at_ = presented_at_ = pause_at_ = monotonic_ms; clock_started_ = true; }
  now_ = monotonic_ms;
  if (rejection_until_ && now_ >= rejection_until_) {
    rejection_until_ = 0;
    ++revision_;
  }
  // A native QR/approval panel can cover an already captured artwork for an
  // arbitrary time. It has loaded successfully but must not enter history or
  // start its duration until uncovered. Navigation alone still times out.
  if (selected_ && !selected_->frame_available && (selected_->stage == "loading" || selected_->stage == "committed") && now_ - selected_at_ > 60000) failed(selected_->id, "Artwork produced no presentable frame within 60 seconds");
  if(now_<storage_retry_at_)return;
  try {
    if(state_.contains("cohortPlan")&&!state_["cohortPlan"].is_null()){
      auto plan=state_["cohortPlan"];Json active;auto list=cohort(plan,wall_time_ms,active);
      if(active!=plan["activeAt"]){
        auto next=state_;plan["activeAt"]=active;
        Json outcome={{"id",uuid()},{"atMs",active.is_null()?Json(0):active},{"executedAtMs",wall_time_ms},{"instance",instance_},{"generation",generation_+1},{"outcome","applied"}};
        bool applied=false;
        if(!list["items"].empty())try{replace(next,{{"playlist",list},{"context",plan["context"]},{"playlistUrl",plan["playlistUrl"]}});applied=true;}
        catch(const Error& error){
          if(error.code!="content_blocked")throw;
          next=state_;outcome["outcome"]="failed";outcome["error"]={{"code",error.code},{"message",error.what()}};
          // A pending projection belongs to the previous cohort. Its slots
          // expire at this boundary even when the new cohort cannot play.
          if (!next.value("pendingUpdate", Json(nullptr)).is_null()) {
            next["updateOutcome"] = {{"id",next["pendingUpdate"]["id"]},{"outcome","failed"},{"contentBlocked",true}};
            next["pendingUpdate"] = nullptr;
          }
        }

        // A cohort boundary advances the current cast; it must not cancel a
        // later explicit schedule accepted while that cast was playing.
        next["schedule"]=state_["schedule"];
        if (applied) plan["started"] = true;
        next["cohortPlan"]=plan;next["scheduleOutcome"]=outcome;persist(std::move(next));++generation_;++revision_;
        if(applied){cast_id_=uuid();select();}
      }
    }
    if (!state_["schedule"].is_null() && state_["schedule"].value("atMs", INT64_MAX) <= wall_time_ms) {
      execute_schedule(wall_time_ms);
    }
    if(!selected_&&state_["stopped"]==false&&state_["sleeping"]==false&&!state_.value("pendingUpdate",Json(nullptr)).is_null())
      apply_pending_update();
    if(selected_ && selected_->stage=="failed" && recovery_attempt_==3 &&
       !state_.value("paused",false) && !state_.value("sleeping",false) && overlay().is_null() &&
       !exhausted_items_.contains(selected_->index)){
      exhausted_items_.insert(selected_->index);
      try { advance(true); }
      catch (...) { exhausted_items_.erase(selected_->index);throw; }
    }
    if(recovery_at_&&now_>=recovery_at_&&selected_&&selected_->stage=="failed"){
      // Selection changes clear the deadline. Reusing the playlist generation
      // preserves its trust binding; a new occurrence rejects every late
      // renderer callback and old input/seek target from the failed attempt.
      recovery_at_=0;++recovery_attempt_;select(true);
    }
    if(recovery_attempt_&&selected_&&presented_&&selected_->id==presented_->id&&now_-presented_at_>=60000){
      clear_recovery();++revision_;
    }
    if(eos_pending_ && selected_ && presented_ && selected_->id==presented_->id && state_["paused"] == false && state_["sleeping"] == false) { advance(); eos_pending_=false; }
    if (selected_ && presented_ && selected_->id == presented_->id && state_["paused"] == false && state_["sleeping"] == false && duration() > 0 && now_ - presented_at_ >= duration() * 1000) advance();
  }catch(const Error& error){
    // A timer is not a request with an error reply. Preserve live playback and
    // the command socket, report the failure in snapshots/UI, and retry at a
    // bounded cadence instead of exiting or hammering an unwritable database.
    if(error.code!="storage_unavailable"&&error.code!="storage_limit")throw;
  }
}
}
