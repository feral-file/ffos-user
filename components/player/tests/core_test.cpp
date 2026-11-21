#include "ff/protocol.hpp"
#include "ff/media_kind.hpp"
#include <filesystem>
#include <fstream>
#include <iostream>
#include <sqlite3.h>

namespace {
int checks = 0;
void check(bool ok, const char* why) { ++checks; if (!ok) throw std::runtime_error(why); }
template <typename F> void fails(F fn, const char* code) {
  try { fn(); } catch (const ff::Error& error) { check(error.code == code, "Unexpected error code"); return; }
  throw std::runtime_error("Expected failure");
}
ff::Json playlist() { return {{"id", "test"}, {"title", "Two occurrences"}, {"items", {{{"id", "a"}, {"source", "https://example.org/art"}, {"duration", 1}}, {{"id", "b"}, {"source", "https://example.org/art"}, {"duration", 1}}}}}; }
}
int main(int argc, char** argv) {
  try {
    check(argc == 2, "Fixture path required"); std::ifstream f(argv[1]); auto fixtures = ff::Json::parse(f);
    check(ff::virtual_cache_source("https://abc.onchfs.invalid/work.png")&&!ff::virtual_cache_source("https://abc.onchfs.invalid.evil/work.png"),"Virtual chain sources always use the owned cache broker");
    check(ff::native_media({{"source","https://example.org/movie.MP4?token=x"}}),"MP4 uses native media despite query");
    check(ff::native_media({{"source","https://example.org/opaque"},{"mimeType","video/mp4"}}),"MIME-resolved video uses native media");
    check(ff::native_media({{"source","https://example.org/opaque"},{"mimeType","application/x-mpegurl"}}),"Normalized HLS MIME uses native media");
    check(ff::native_media({{"source","https://example.org/opaque"},{"mimeType","Video/MP4"}}),"Media MIME comparison is case insensitive");
    check(ff::native_media({{"source","https://example.org/live.m3u8"}}),"HLS uses native media");
    for(auto ext:{"PNG","jpeg","GIF","WEBP","avif","tiff"})check(ff::native_raster({{"source",std::string("https://example.org/image.")+ext+"?token=1#x"}}),"Raster image uses native decoder");
    check(ff::native_raster({{"source","https://example.org/opaque"},{"mimeType","Image/PNG"}}),"MIME-only raster uses native decoder");
    check(!ff::native_raster({{"source","https://example.org/work.SVG?x"},{"mimeType","image/png"}}),"SVG extension stays a browser document even with a conflicting raster type");
    check(!ff::native_raster({{"source","https://example.org/opaque"},{"mimeType","image/svg+xml"}}),"SVG MIME stays a browser document");
    check(!ff::native_media({{"source","https://example.org/artwork/"}}),"Software stays a browser document");
    for (const auto& fixture : fixtures["parse"]) {
      bool valid = true; try { ff::parse(fixture["wire"].get<std::string>()); } catch (const ff::Error&) { valid = false; }
      check(valid == fixture["valid"], "Shared parsing fixture failed");
    }
    auto directory = std::filesystem::temp_directory_path() / ("ff-native-test-" + ff::uuid());
    std::filesystem::create_directory(directory);
    auto path = directory / "state.sqlite";
    {
      ff::Store store(path);
      auto old = ff::initial_state();
      old["artworkPermissions"] = {{"camera", true}};
      store.save(old);
      ff::Playback p(store);
      auto grants = p.command("artworkPermissions.get", ff::Json::object());
      check(grants.at("autoplay") == true && grants.at("localNetwork") == false &&
            grants.at("geolocation") == false, "Old devices inherit kiosk defaults");
      check(grants.at("camera") == true && !store.load().contains("artworkPermissions"),
            "Legacy grant moves to the global file and leaves SQLite");
      fails([&]{p.command("artworkPermissions.set", {{"geolocation","yes"}});}, "invalid_params");
      fails([&]{p.command("artworkPermissions.set", {{"playlistId","x"}});}, "invalid_params");
      check(grants["cache"] == ff::Json{{"maxAgeDays",30},{"maxBytes",1073741824}}, "Legacy policy inherits cache limits");
      fails([&]{p.command("artworkPermissions.set", {{"cache",{{"maxBytes",-1}}}});}, "invalid_params");
      p.command("artworkPermissions.set", {{"cache",{{"maxAgeDays",14}}}});
      p.command("artworkPermissions.set", {{"notifications",true}});
    }
    {
      ff::Store store(path); ff::Playback p(store); p.tick(1000, ff::wall_ms());
      check(p.global_policy()["cache"] == ff::Json{{"maxAgeDays",14},{"maxBytes",1073741824}}, "Partial cache patch survives restart");
      check(p.snapshot()["artworkPermissions"]["notifications"] == true, "Global grant survives restart");
      fails([&]{ff::Store second_owner(path);},"storage_in_use");
      p.command("connectivity.set", {{"online",false}});
      p.command("overlay.set", {{"owner","setup"},{"state","connecting"}});
      auto result = p.command("playback.replace", {{"playlist", playlist()}});
      check(result["outcome"] == "accepted", "Acceptance must not claim presentation");
      auto a = p.desired()->id; check(!p.visible(), "No frame has been presented");
      p.presented(a); check(!p.visible(), "Cannot present before commit");
      p.committed(a);
      check(!p.overlay().is_null(), "Reconnect advisory stays until a frame is ready");
      p.frame_ready(a);
      check(p.overlay().is_null(), "Offline artwork can reach first presentation");
      p.command("overlay.set", {{"owner","setup"},{"state","joining"}});
      check(!p.overlay().is_null(), "Interactive setup keeps ownership");
      p.command("overlay.set", {{"owner","setup"},{"state","connecting"}});
      p.presented(a); check(p.visible()->id == a, "First occurrence presented");
      double seek=-1;p.set_seek_handler([&](const std::string& id,double seconds){check(id==a,"Seek is occurrence scoped");seek=seconds;});
      check(p.command("media.seek",{{"occurrence",a},{"seconds",2.5}})["outcome"]=="accepted"&&seek==2.5,"Seek acceptance is not completion");
      fails([&]{p.command("media.seek",{{"occurrence","old"},{"seconds",0}});},"stale_occurrence");
      p.tick(2001, ff::wall_ms()); auto b = p.desired()->id;
      check(a != b && p.desired()->index == 1, "Same URL must be a new occurrence");
      check(p.visible()->id == a, "Outgoing remains separately identified");
      check(p.overlay().is_null(), "Reconnect advisory stays hidden during offline handoff");
      p.command("overlay.set", {{"owner","setup"},{"state","hidden"}});
      p.committed(a); check(p.desired()->stage == "loading", "Stale callback ignored");
      p.committed(b); p.presented(b);
      p.command("playback.mode", {{"loop", "none"}}); p.tick(4000, ff::wall_ms());
      check(p.desired()->id == b, "Repeat off holds final item");
      auto mature = playlist(); for (auto& item : mature["items"]) item["contentRating"] = "mature";
      fails([&] { p.command("playback.replace", {{"playlist", mature}}); }, "content_blocked");
      check(p.desired()->id == b, "Blocked cast preserves current work");
      p.command("playback.sleep", {{"sleeping", true}}); check(!p.visible(), "Sleep releases presentation");
      p.command("playback.sleep", {{"sleeping", false}}); check(p.desired()->id != b, "Wake starts new occurrence");
      auto history = p.command("history.list", ff::Json::object()); check(history["records"].size() == 2, "History records only presented occurrences");
      ff::Protocol wire(p); ff::Connection c;
      auto hello = wire.receive(c, ff::parse(R"({"v":1,"id":"h","method":"hello","params":{"client":"test","versions":[1]}})"));
      check(hello.contains("result"), "Negotiate");
      for(const auto& fixture:fixtures["requests"]){
        ff::Json request={{"v",1},{"id","fixture"},{"method","state.get"},{"params",ff::Json::object()},
          {"instance",p.instance()},{"generation",p.generation()},{"key",ff::uuid()},{"deadlineMs",ff::wall_ms()+1000}};
        request.update(fixture["overrides"]);
        auto response=wire.receive(c,request);
        if(!response.contains("error")||response["error"]["code"]!=fixture["error"])
          throw std::runtime_error("Shared request fixture failed: "+fixture["name"].get<std::string>()+" "+response.dump());
      }
      auto invalid_hello=ff::parse(R"({"v":1,"id":"bad-hello","method":"hello","params":{"client":"different","versions":[1.0]}})");
      check(wire.receive(c,invalid_hello)["error"]["code"]=="version_mismatch"&&c.client=="test",
        "Failed renegotiation cannot change the receipt namespace or accept a float version");
      fails([&]{p.command("playback.select",{{"index",uint64_t(4294967296)}});},"invalid_params");
      fails([&]{p.command("playback.replace",{{"playlist",playlist()},{"index",uint64_t(4294967296)}});},"invalid_params");
      ff::Json request = {{"v", 1}, {"id", "r"}, {"method", "playback.refresh"}, {"params", ff::Json::object()}, {"instance", p.instance()}, {"generation", p.generation()}, {"deadlineMs", ff::wall_ms() + 1000}, {"key", "once"}};
      auto reply = wire.receive(c, request); auto id = p.desired()->id;
      check(reply.contains("result"), "Mutation accepted");
      request["id"] = "again"; check(wire.receive(c, request)["result"] == reply["result"] && p.desired()->id == id, "Duplicate returns receipt without replay");
      request["method"] = "playback.stop"; check(wire.receive(c, request)["error"]["code"] == "idempotency_conflict", "Conflicting retry fails");
      request["key"] = "new"; request["generation"] = uint64_t(0); check(wire.receive(c, request)["error"]["code"] == "stale_generation", "Stale operation fails");
      sqlite3* db; sqlite3_open(path.c_str(), &db); sqlite3_exec(db, "BEGIN EXCLUSIVE", nullptr, nullptr, nullptr);
      fails([&] { p.command("settings.set", {{"tombstone", "off"}}); }, "storage_unavailable");
      check(p.state()["settings"]["tombstone"] == "timed", "Failed persistence does not change live setting");
      check(p.snapshot()["storage"]["health"]=="write_failed"&&!p.storage_error().empty(),"Storage failure is visible to status and native UI");
      sqlite3_exec(db, "ROLLBACK", nullptr, nullptr, nullptr); sqlite3_close(db);
      auto manual = playlist(); manual["items"][0]["display"] = {{"autoPlay",false}};
      p.command("playback.replace",{{"playlist",manual}});
      check(p.media_paused(),"An autoPlay false item remains paused despite periodic controls");
      p.command("settings.set",{{"volume",0.5}});
      check(p.storage_error().empty(),"Successful durable write clears storage failure");
      check(p.media_paused(),"Volume does not override autoPlay false");
      p.command("playback.pause",{{"paused",false}});
      check(!p.media_paused(),"Explicit resume releases the autoplay hold");
      auto active=p.desired()->id;p.committed(active);p.presented(active);
      p.committed(active);
      int input_count=0;p.set_input_handler([&](const ff::Json&){++input_count;});
      p.command("input.dispatch",{{"kind","move"},{"occurrence",active},{"dx",10},{"referenceWidth",1920}});
      check(input_count==1,"Reference-width movement accepted at protocol boundary");
      for(auto width:{0,16385})fails([&]{p.command("input.dispatch",{{"kind","move"},{"occurrence",active},{"dx",10},{"referenceWidth",width}});},"invalid_params");
      fails([&]{p.command("input.dispatch",{{"kind","move"},{"occurrence",active},{"x",10},{"referenceWidth",1920}});},"invalid_params");
      check(input_count==1,"Invalid coordinate units dispatch no input");
      check(p.desired()->stage=="presented","Seek restart does not demote a presented occurrence");
      auto incoming=playlist();incoming["title"]="Incoming exhibition";
      p.command("playback.replace",{{"playlist",incoming}});
      check(p.visible()->playlist_title=="Two occurrences"&&p.desired()->playlist_title=="Incoming exhibition",
        "Outgoing label keeps its exhibition while another playlist loads");
      check(p.snapshot()["presentedPlaylist"]["title"]=="Two occurrences"&&
        p.snapshot()["playlist"]["title"]=="Incoming exhibition",
        "Status keeps the outgoing admitted playlist until the incoming occurrence is presented");
      check(p.snapshot()["presented"]["generation"]!=p.snapshot()["generation"],
        "Outgoing verification must remain bound to its own generation");
      p.command("settings.set",{{"framing","fill"}});
      check(p.desired()->display["scaling"]=="fill","Saved owner framing applies to current selection");
      check(!p.desired()->item.contains("display"),"Owner framing never mutates the signed item");
      incoming["items"][0]["display"]={{"userOverrides",false},{"scaling","fit"}};
      p.command("playback.replace",{{"playlist",incoming}});
      check(p.desired()->display["scaling"]=="fit","Authored override restriction wins over owner framing");
      auto retained=p.desired()->id;
      incoming["items"][0]["inlineManifest"]={{"controls",ff::Json::array()}};
      fails([&]{p.command("playback.replace",{{"playlist",incoming}});},"playlist_invalid");
      check(p.desired()->id==retained,"Malformed nested display input cannot replace live state");
      auto completion=playlist();completion["items"][0]["duration"]=0;completion["items"][0]["display"]={{"loop",false}};
      p.command("playback.replace",{{"playlist",completion}});auto ending=p.desired()->id;p.committed(ending);p.presented(ending);
      sqlite3_open(path.c_str(),&db);sqlite3_exec(db,"BEGIN IMMEDIATE",nullptr,nullptr,nullptr);
      p.ended(ending);p.tick(5000,ff::wall_ms());
      check(p.desired()->id==ending&&p.snapshot()["storage"]["health"]=="write_failed","EOS write failure retains playback and leaves the command interface alive");
      p.tick(6000,ff::wall_ms());check(p.desired()->id==ending,"Storage retries are paced");
      sqlite3_exec(db,"ROLLBACK",nullptr,nullptr,nullptr);sqlite3_close(db);
      p.tick(10001,ff::wall_ms());check(p.desired()->index==1&&p.storage_error().empty(),"Pending EOS advances after storage recovers");
      auto failed_id=p.desired()->id;p.failed(failed_id,"first failure");auto failed_revision=p.revision();
      p.failed(failed_id,"repeated callback");check(p.revision()==failed_revision&&p.desired()->error=="first failure","Repeated renderer failure cannot flood revisions or replace the root cause");
    }
    {
      ff::Store store(path); ff::Playback p(store); check(p.desired().has_value() && !p.visible(), "Restart restores intent without fabricated presentation");
      p.tick(999999999, ff::wall_ms()); check(p.desired()->stage == "loading", "Boot clock does not cause immediate timeout");
    }
    {
      std::ofstream(directory/"global-policy.json") << R"({"localNetwork":true})";
      ff::Store store(path);
      auto interrupted = store.load();
      interrupted["artworkPermissions"] = {{"notifications",true}};
      store.save(interrupted);
      ff::Playback p(store);
      check(!store.load().contains("artworkPermissions"), "Restart finishes interrupted SQLite cleanup");
      check(p.global_policy().at("localNetwork") && !p.global_policy().at("notifications"),
            "Manual edit replaces saved grants on restart");
    }
    {
      std::ofstream(directory/"global-policy.json") << R"({"localNetwork":true,"camera":1})";
      ff::Store store(path); ff::Playback p(store);
      check(!p.global_policy().at("localNetwork"), "Invalid file discards all optional grants");
      std::filesystem::remove(directory/"global-policy.json");
      std::filesystem::create_directory(directory/"global-policy.json");
      fails([&]{p.command("artworkPermissions.set", {{"localNetwork",true}});}, "storage_unavailable");
      check(!p.global_policy().at("localNetwork"), "Failed publication cannot grant access");
    }
    std::filesystem::remove_all(directory);
    std::cout << checks << " checks passed\n";
  } catch (const std::exception& e) { std::cerr << "FAIL: " << e.what() << '\n'; return 1; }
}
