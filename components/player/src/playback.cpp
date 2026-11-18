#include "ff/playback.hpp"
#include "ff/input.hpp"
#include "ff/overlay.hpp"
#include "ff/display.hpp"
#include <algorithm>
#include <cmath>
#include <numeric>
#include <random>

namespace ff {
namespace {
Json occurrence_json(const std::optional<Occurrence>& occurrence) {
  if (!occurrence) return nullptr;
  const auto& o = *occurrence;
  return {{"id", o.id}, {"generation", o.generation}, {"index", o.index}, {"item", o.item},
          {"display", o.display}, {"displayRevision",o.display_revision}, {"stage", o.stage}, {"error", o.error}, {"navigationCommitted", o.navigation_committed},
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
void validate_display(const Json& p){
  require(p.is_object(),"playlist_invalid","Display must be an object");
  for(auto key:{"autoPlay","loop","userOverrides"})if(p.contains(key))require(p[key].is_boolean(),"playlist_invalid","Display flags must be booleans");
  if(p.contains("scaling"))require(p["scaling"]=="fit"||p["scaling"]=="fill"||p["scaling"]=="stretch"||p["scaling"]=="auto","playlist_invalid","Invalid display scaling");
  optional_text(p,"background",64);
  if(p.contains("background"))display_color(p["background"].get<std::string>());
  if(p.contains("margin")){
    const auto& margin=p["margin"];
    bool valid=margin.is_number()&&margin.get<double>()>=0&&margin.get<double>()<=16384;
    if(margin.is_string()){
      auto text=margin.get<std::string>();
      if(text.size()<=16&&text.ends_with('%'))try{
        size_t consumed=0;auto value=std::stod(text,&consumed);
        valid=consumed==text.size()-1&&std::isfinite(value)&&value>=0&&value<=100;
      }catch(const std::exception&){}
    }
    require(valid,"playlist_invalid","Invalid display margin");
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
void validate_layer(const Json& p){
  require(p.is_object(),"playlist_invalid","Manifest/defaults/override must be an object");
  if(p.contains("display"))validate_display(p["display"]);
  if(p.contains("metadata"))validate_metadata(p["metadata"]);
  if(p.contains("controls")){
    require(p["controls"].is_object(),"playlist_invalid","Manifest controls must be an object");
    if(p["controls"].contains("display"))validate_display(p["controls"]["display"]);
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
}
Playback::Playback(Store& store) : store_(store), state_(store.load()) {
  // Validate before exposing a socket. Corruption must not start an empty cast.
  auto expected = initial_state();
  for (auto it = expected.begin(); it != expected.end(); ++it)
    require(state_.contains(it.key()) && (it.value().is_null() || (it.value().is_number_integer() && state_[it.key()].is_number_integer()) || state_[it.key()].type() == it.value().type()), "storage_corrupt", "Invalid persistent state field: " + it.key());
  if (!state_["playlist"].is_null()) validate_playlist(state_["playlist"]);
  validate_settings(state_["settings"]);
  validate_state();
  if(state_.contains("pendingUpdate")&&!state_["pendingUpdate"].is_null()){
    const auto& pending=state_["pendingUpdate"];
    fields(pending,{"id","playlist","context"});string_field(pending,"id",64);
    require(pending.contains("playlist"),"storage_corrupt","Pending refresh has no playlist");
    validate_playlist(pending["playlist"]);
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
    validate_playlist({{"items",Json::array({record["item"]})}});
    require(record.value("context","curated")=="curated"||record.value("context","curated")=="personal","storage_corrupt","Invalid history context");
    if(record.contains("defaults"))validate_layer(record["defaults"]);
  }
  for(auto key:{"boot","schedule"}){
    const auto& entry=state_[key];if(entry.is_null())continue;
    fields(entry,{"playlist","context","atMs","clear","id"});
    if(entry.contains("id"))string_field(entry,"id",64);
    require(entry.contains("playlist"),"storage_corrupt","Stored boot/schedule playlist is missing");validate_playlist(entry["playlist"]);
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
void Playback::validate_playlist(const Json& playlist) const {
  require(playlist.is_object() && playlist.contains("items") && playlist["items"].is_array(), "playlist_invalid", "Playlist must contain items");
  require(playlist["items"].size() <= 4096, "playlist_invalid", "At most 4096 items are allowed");
  // Leave room for occurrence snapshots and durable history within 4 MiB.
  require(playlist.dump().size()<=1024*1024,"playlist_invalid","Playlist exceeds 1 MiB");
  for(auto key:{"id","title","curator","dpVersion","slug","created"})optional_text(playlist,key);
  if(playlist.contains("defaults"))validate_layer(playlist["defaults"]);
  for (const auto& item : playlist["items"]) {
    require(item.is_object(), "playlist_invalid", "Every item must be an object");
    require(item.dump().size()<=65536,"playlist_invalid","Artwork item exceeds 64 KiB");
    optional_text(item,"mimeType",256);
    for(auto key:{"id","title","slug","license","ref","refHash"})optional_text(item,key);
    auto source = string_field(item, "source", 16384);
    require(source.starts_with("https://") || source.starts_with("http://") || source.starts_with("data:") || source.starts_with("ipfs://") || source.starts_with("onchfs://"), "playlist_invalid", "Unsupported artwork URL scheme");
    if (item.contains("contentRating")) require(item["contentRating"].is_string(), "playlist_invalid", "contentRating must be a string");
    if (item.contains("contentReasons")) {
      require(item["contentReasons"].is_array(), "playlist_invalid", "contentReasons must be an array");
      for (const auto& reason : item["contentReasons"]) require(reason.is_string(), "playlist_invalid", "contentReasons must contain strings");
    }
    if (item.contains("duration")) require(item["duration"].is_number() && item["duration"].get<double>() >= 0, "playlist_invalid", "duration must be nonnegative");
    if(item.contains("display"))validate_display(item["display"]);
    if(item.contains("metadata"))validate_metadata(item["metadata"]);
    for (auto key : {"override", "inlineManifest", "resolvedManifest"}) if(item.contains(key))validate_layer(item[key]);
  }
}
Json Playback::display(const Json& item, const Json& defaults, const Json& adjustment) const {
  Json result = {{"scaling", state_["settings"].value("scaling", "fit")}, {"margin", state_["settings"].value("margin",Json(0))}, {"background", state_["settings"].value("background","#000000")}, {"autoPlay", true}, {"loop", true}, {"userOverrides", true}};
  const auto merge = [&](const Json& layer) { if (layer.is_object()) result.update(layer); };
  merge(defaults.value("display", Json::object()));
  merge(item.value("inlineManifest", Json::object()).value("controls", Json::object()).value("display", Json::object()));
  // resolvedManifest is supplied by controld after its bounded fetch/hash check.
  merge(item.value("resolvedManifest", Json::object()).value("controls", Json::object()).value("display", Json::object()));
  merge(item.value("override", Json::object()).value("display", Json::object()));
  merge(item.value("display", Json::object()));
  if(result.value("userOverrides",true))merge(adjustment);
  auto framing=state_["settings"].value("framing","artwork");
  if(result.value("userOverrides",true)&&framing!="artwork")result["scaling"]=framing;
  return result;
}
void Playback::release_input() { if (input_) input_({{"kind", "releaseAll"}}); }
void Playback::clear_recovery() {
  recovery_attempt_=0;recovery_at_=0;recovery_error_.clear();recovery_occurrence_.clear();
}
void Playback::select(bool recovering) {
  if(!recovering){clear_recovery();presentation_failure_=nullptr;}
  release_input();
  eos_pending_ = false;
  selected_.reset();
  media_status_ = nullptr;
  ++revision_;
  if (state_["playlist"].is_null() || state_["stopped"] == true || state_["sleeping"] == true) { presented_.reset(); return; }
  const auto& items = state_["playlist"]["items"];
  if (items.empty()) { presented_.reset(); return; }
  int index = state_["index"].get<int>();
  require(index >= 0 && index < static_cast<int>(items.size()), "storage_corrupt", "Selection is outside playlist");
  if (!allowed(items[index], state_)) {
    auto found = std::find_if(items.begin(), items.end(), [&](const Json& item) { return allowed(item, state_); });
    if (found == items.end()) { presented_.reset(); return; }
    auto next = state_; next["index"] = index = static_cast<int>(found - items.begin()); persist(std::move(next));
  }
  auto defaults=state_["playlist"].value("defaults",Json::object());
  selected_ = Occurrence{uuid(), items[index], display(items[index],defaults), index, generation_, "loading", "", false,
    defaults,state_["context"].get<std::string>(),state_["playlist"].value("title",""),state_["playlist"].value("curator",""),
    state_["playlist"],state_.value("playlistUrl", ""),cast_id_};
  if(!recovering)autoplay_hold_ = !selected_->display.value("autoPlay", true);
  selected_at_ = now_;
}
bool Playback::media_paused(const std::string& occurrence) const {
  // Decode the first frame while paused, then start only once the host has
  // actually exposed it. Setup may cover the incoming work for minutes.
  const bool waiting=!occurrence.empty()&&(!presented_||presented_->id!=occurrence);
  return state_["paused"] == true || autoplay_hold_ || waiting;
}
void Playback::replace(Json& next, const Json& p) {
  fields(p, {"playlist", "index", "context", "playlistUrl"});
  require(p.contains("playlist"), "invalid_params", "Missing playlist");
  validate_playlist(p["playlist"]);
  optional_text(p,"playlistUrl",16384);
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
  next["stopped"] = items.empty(); next["paused"] = false; next["schedule"] = nullptr;
  next["scheduleOutcome"] = nullptr;
  next["pendingUpdate"] = nullptr; next["updateOutcome"] = nullptr;
  // Each explicit cast starts a fresh playback mode, matching CanvasService.
  next["shuffle"] = false; next["loop"] = "playlist";
  std::vector<int> order(items.size()); std::iota(order.begin(), order.end(), 0);
  next["order"] = order;
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
  if (method == "policy.get") { fields(p, {}); return state_["policy"]; }
  if (method == "playback.update") return update_playlist(p);
  if (method == "connectivity.set") { fields(p, {"online"}); online_ = boolean(p, "online"); ++revision_; return {{"outcome", "applied"}}; }
  if (method == "overlay.set") {
    validate_overlay(p);
    auto owner=p["owner"].get<std::string>(),state=p["state"].get<std::string>();
    // Legacy ready is a non-visible setup state, not a success title panel.
    if(state=="hidden" || (owner=="setup" && state=="ready")) {
      if(!overlay_.is_null() && overlay_["owner"]==owner)overlay_=nullptr;
    } else { release_input(); overlay_=p; }
    ++revision_; return {{"outcome", "applied"}};
  }
  if (method == "input.dispatch") {
    require(p.contains("occurrence") && presented_ && p["occurrence"] == presented_->id, "stale_occurrence", "Input target is not currently presented");
    require(overlay_.is_null() && state_["sleeping"] == false && (!selected_ || selected_->stage!="failed") && !composition_pending(), "input_blocked", "Device UI or a pending presentation change owns focus");
    require(static_cast<bool>(input_), "unavailable", "No presentation backend");
    auto kind=string_field(p,"kind",32);
    if(kind=="move"){
      fields(p,{"kind","occurrence","x","y","dx","dy"});
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
    require(selected_->display.value("userOverrides",true),"presentation_locked","Artwork does not allow presentation overrides");
    require(p.contains("display"),"invalid_params","Missing presentation settings");
    auto adjustment=p["display"];fields(adjustment,{"scaling","margin","background"});validate_display(adjustment);
    auto colors=adjustment;colors.erase("margin");validate_settings(colors);
    selected_->adjustment.update(adjustment);selected_->display=display(selected_->item,selected_->defaults,selected_->adjustment);++selected_->display_revision;
    presented_->adjustment=selected_->adjustment;presented_->display=selected_->display;presented_->display_revision=selected_->display_revision;
    ++revision_;
    return {{"outcome","accepted"},{"generation",generation_},{"revision",revision_},{"occurrence",id},{"acceptedCompositionRevision",composition_revision_}};
  }
  auto next = state_;
  bool reselection = false, generation = false;
  if (method == "playback.replace") { replace(next, p); reselection = generation = true; }
  else if (method == "playback.select") {
    fields(p, {"index"}); require(!next["playlist"].is_null() && p.contains("index") && p["index"].is_number_integer(), "invalid_params", "Missing playlist or index");
    require(p["index"] >= 0 && p["index"] <= 4096, "invalid_params", "Invalid playlist index");
    int index = p["index"].get<int>();
    require(index >= 0 && index < static_cast<int>(next["playlist"]["items"].size()), "invalid_params", "Index out of bounds");
    require(allowed(next["playlist"]["items"][index], next), "content_blocked", "Selected work is blocked");
    next["index"] = index; next["stopped"] = false; reselection = true;
  } else if (method == "playback.refresh") { fields(p, {}); reselection = true; }
  else if (method == "playback.stop" || method == "playback.cancel") { fields(p, {}); next["stopped"] = true; next["pendingUpdate"]=nullptr; next["updateOutcome"]=nullptr; reselection = generation = true; }
  else if (method == "playback.sleep") { fields(p, {"sleeping"}); next["sleeping"] = boolean(p, "sleeping"); reselection = true; }
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
    reselection = !selected_ || !allowed(selected_->item, next);
  } else if (method == "schedule.set" || method == "boot.set") {
    fields(p, {"playlist", "context", "atMs", "clear"});
    auto key = method == "boot.set" ? "boot" : "schedule";
    if (p.value("clear", false)) next[key] = nullptr;
    else {
      require(p.contains("playlist"), "invalid_params", "Missing playlist"); validate_playlist(p["playlist"]);
      require(p.value("context","curated")=="curated"||p.value("context","curated")=="personal","invalid_params","Invalid content context");
      if (method == "schedule.set") require(p.contains("atMs") && p["atMs"].is_number_integer() && p["atMs"].get<int64_t>() > 0, "invalid_params", "Invalid schedule timestamp");
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
    if(!allowed(presented_->item,presented_policy)){release_input();presented_.reset();}
  }
  if (method == "playback.pause") autoplay_hold_ = false;
  if (method == "settings.set") {
    if(selected_){selected_->display=display(selected_->item,selected_->defaults,selected_->adjustment);++selected_->display_revision;}
    if(presented_){presented_->display=display(presented_->item,presented_->defaults,presented_->adjustment);++presented_->display_revision;}
  }
  if (generation) { ++generation_; cast_id_=uuid(); }
  if (reselection) select();
  if (state_["paused"] == true && !was_paused) pause_at_ = now_;
  if (state_["paused"] == false && was_paused) presented_at_ += now_ - pause_at_;
  return {{"outcome", reselection && selected_ ? "accepted" : "applied"}, {"generation", generation_}, {"revision", revision_}, {"occurrence", selected_ ? Json(selected_->id) : Json(nullptr)}};
}
Json Playback::snapshot() const {
  return {{"instance", instance_}, {"generation", generation_}, {"revision", revision_},
          {"host", "responsive"}, {"renderer", selected_ ? selected_->stage : "absent"},
          {"storage", {{"health",storage_error_.empty()?"available":"write_failed"},{"error",storage_error_}}},
          {"presentationFailure",presentation_failure_},
          {"composition",presented_&&!composition_.is_null()&&composition_.value("occurrence","")==presented_->id?composition_:Json(nullptr)},
          {"recovery",{{"attempt",recovery_attempt_},{"limit",3},{"pending",recovery_at_!=0},
            {"exhausted",selected_&&selected_->stage=="failed"&&recovery_attempt_==3},
            {"lastError",recovery_error_},{"failedOccurrence",recovery_occurrence_},
            {"retryDelayMs",recovery_at_?1000<<recovery_attempt_:0}}},
          {"selected", occurrence_json(selected_)}, {"presented", occurrence_json(presented_)},
          {"presentedPlaylist", presented_ ? presented_->playlist : Json(nullptr)},
          {"presentedPlaylistUrl", presented_ ? presented_->playlist_url : ""},
          {"stopped", state_["stopped"]}, {"sleeping", state_["sleeping"]}, {"paused", state_["paused"]},
          {"playlist", state_["playlist"]}, {"playlistUrl", state_.value("playlistUrl", "")},
          {"loop", state_["loop"]}, {"shuffle", state_["shuffle"]}, {"settings", state_["settings"]},
          {"context", state_["context"]}, {"overlay", overlay_}, {"online", online_}, {"media",media_status_}, {"mediaPaused",media_paused()},
          {"schedule",state_["schedule"]},{"scheduleOutcome",state_.value("scheduleOutcome",Json(nullptr))},
          {"pendingUpdate",state_.value("pendingUpdate",Json(nullptr)).is_null()?Json(nullptr):Json{
            {"id",state_["pendingUpdate"]["id"]},{"itemCount",state_["pendingUpdate"]["playlist"]["items"].size()}}},
          {"updateOutcome",state_.value("updateOutcome",Json(nullptr))}};
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
void Playback::composed(const std::string& id,uint64_t display_revision){
  // Only the host calls this, after submitting a frame with this configuration.
  // An old buffer/configuration acknowledgement cannot confirm a later write.
  if(!presented_||presented_->id!=id||presented_->display_revision!=display_revision)return;
  if(!composition_.is_null()&&composition_.value("occurrence","")==id&&composed_display_revision_==display_revision)return;
  composed_display_revision_=display_revision;++composition_revision_;++revision_;
  composition_={{"occurrence",id},{"revision",composition_revision_},{"displayRevision",display_revision},{"display",presented_->display}};
}
void Playback::presented(const std::string& id) {
  if (!selected_ || selected_->id != id || !selected_->navigation_committed || selected_->stage == "failed" || (presented_ && presented_->id == id)) return;
  selected_->stage = "presented"; presented_ = selected_; presented_at_ = now_; ++revision_;
  presentation_failure_=nullptr;
  if(state_["paused"]==true)pause_at_=now_;
  auto next = state_;
  Json record = {{"recordId", id}, {"playedAtMs", wall_ms()}, {"item", selected_->item}, {"context", selected_->context}, {"defaults", selected_->defaults}};
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
  if (selected_->display.value("loop", true) && selected_->display.value("userOverrides", true) && state_["settings"].contains("durationSeconds") && !state_["settings"]["durationSeconds"].is_null())
    return state_["settings"]["durationSeconds"].get<double>();
  return selected_->item.value("duration", 0.0);
}
void Playback::advance() {
  if (!selected_ || state_["paused"] == true) return;
  if(apply_pending_update())return;
  auto next = state_; const auto& items = next["playlist"]["items"];
  auto order = next["order"].get<std::vector<int>>();
  if (order.size() != items.size()) { order.resize(items.size()); std::iota(order.begin(), order.end(), 0); }
  if (state_["shuffle"] == false) std::iota(order.begin(), order.end(), 0);
  auto it = std::find(order.begin(), order.end(), selected_->index);
  size_t pos = it == order.end() ? 0 : static_cast<size_t>(it - order.begin());
  for (size_t n = 0; n < order.size(); ++n) {
    if (state_["loop"] != "one") {
      if (pos + 1 == order.size() && state_["loop"] == "none") return;
      pos = (pos + 1) % order.size();
    }
    if (allowed(items[order[pos]], next)) { next["index"] = order[pos]; persist(std::move(next)); select(); return; }
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
  bool admitted=false;
  try{
    auto params=task;params.erase("atMs");params.erase("clear");params.erase("id");
    replace(next,params);admitted=true;outcome["generation"]=generation_+1;
  }catch(const Error& error){
    // A policy change can invalidate an already scheduled playlist. Retire
    // the task atomically, retain current playback, and expose the failure.
    // Leaving a permanently due task would crash/retry on every host restart.
    next=state_;next["schedule"]=nullptr;outcome["outcome"]="failed";
    outcome["error"]={{"code",error.code},{"message",error.what()}};
  }
  next["scheduleOutcome"]=outcome;persist(std::move(next));
  if(admitted){++generation_;cast_id_=uuid();select();}
}
void Playback::tick(int64_t monotonic_ms, int64_t wall_time_ms) {
  if (!clock_started_) { selected_at_ = presented_at_ = pause_at_ = monotonic_ms; clock_started_ = true; }
  now_ = monotonic_ms;
  // A native QR/approval panel can cover an already captured artwork for an
  // arbitrary time. It has loaded successfully but must not enter history or
  // start its duration until uncovered. Navigation alone still times out.
  if (selected_ && !selected_->frame_available && (selected_->stage == "loading" || selected_->stage == "committed") && now_ - selected_at_ > 60000) failed(selected_->id, "Artwork produced no presentable frame within 60 seconds");
  if(now_<storage_retry_at_)return;
  try {
    if (!state_["schedule"].is_null() && state_["schedule"].value("atMs", INT64_MAX) <= wall_time_ms) {
      execute_schedule(wall_time_ms);
    }
    if(!selected_&&state_["stopped"]==false&&state_["sleeping"]==false&&!state_.value("pendingUpdate",Json(nullptr)).is_null())
      apply_pending_update();
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
