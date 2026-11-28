#include "ff/playback.hpp"
#include <filesystem>
#include <iostream>
#include <sqlite3.h>

namespace {
void check(bool condition,const char* reason){if(!condition)throw std::runtime_error(reason);}
ff::Json playlist(std::string rating="general"){
  return {{"items",{{{"source","https://example.org/a"},{"duration",1},{"contentRating",rating}},
    {{"source","https://example.org/b"},{"duration",1},{"contentRating",rating}}}}};
}
}
int main(){
  auto directory=std::filesystem::temp_directory_path()/("ff-schedule-test-"+ff::uuid());
  std::filesystem::create_directory(directory);auto path=(directory/"state.sqlite").string();
  try{
    {
      const auto db=(directory/"migrate.sqlite").string();
      {
        ff::Store store(db); ff::Playback p(store);
        p.import_legacy({{"contentPolicy",nullptr},{"boot_playlist",playlist()},
                         {"castInfo",{{"playlist",playlist()},{"index",1},{"isPaused",true},
                                      {"loopMode","one"},{"shuffle",true},{"contentContext","personal"}}}});
      }
      ff::Store store(db); ff::Playback p(store); p.system_boot("first-native-boot");
      check(p.desired() && p.desired()->index==1 && !p.visible(),"Restore exact slot, await a fresh frame");
      check(p.state()["paused"] && p.state()["loop"]=="one" && p.state()["context"]=="personal",
            "Restore legacy playback intent");
      check(p.state()["shuffle"] && p.state()["order"]==ff::Json::array({0,1}),
            "A migrated shuffle keeps the web player's persisted sequence");
      p.system_boot("next-boot");
      check(p.desired() && p.desired()->index==0,"Subsequent boot uses saved boot playlist");
    }
    {
      ff::Store store(path);ff::Playback p(store);p.tick(1000,1000);
      p.command("playback.replace",{{"playlist",playlist()}});auto id=p.desired()->id;p.committed(id);p.presented(id);
      p.packaged_engine({{"identity", "packaged"}});
      p.engine_status(id, {{"imageSHA256", "current"}});
      p.engine_status("unrelated-worker", {{"imageSHA256", "stale"}});
      check(p.snapshot()["engine"].value("imageSHA256","") == "current", "Unrelated worker replaced the selected engine");
      p.command("schedule.set",{{"playlist",playlist("mature")},{"atMs",2000}});
      p.tick(1001,2000);
      check(p.desired()->id==id&&p.visible()->id==id,"Blocked schedule must retain presentation");
      auto state=p.snapshot();
      check(state["schedule"].is_null()&&state["scheduleOutcome"]["outcome"]=="failed","Blocked due task must retire with explicit failure");
      check(state["scheduleOutcome"]["error"]["code"]=="content_blocked","Schedule must expose policy failure");
      auto revision=p.revision();p.tick(1002,3000);check(p.revision()==revision,"Failed schedule must not hot-loop");
      p.command("policy.set",{{"version",1},{"showMatureContent",true},{"strictPersonal",false},{"blockUnratedCurated",false}});
      p.command("schedule.set",{{"playlist",playlist("mature")},{"atMs",4000}});
      p.command("policy.set",{{"version",1},{"showMatureContent",false},{"strictPersonal",false},{"blockUnratedCurated",false}});
      p.tick(1003,4000);
      check(p.desired()->id==id&&p.snapshot()["scheduleOutcome"]["outcome"]=="failed","Policy tightened after scheduling must not kill host or replace current artwork");
    }
    {
      ff::Store store(path);ff::Playback p(store);p.tick(5000,4000);
      check(p.snapshot()["scheduleOutcome"]["outcome"]=="failed","Schedule failure must survive restart");
      p.command("schedule.set",{{"playlist",playlist()},{"atMs",5000}});
      sqlite3* db=nullptr;sqlite3_open(path.c_str(),&db);sqlite3_exec(db,"BEGIN IMMEDIATE",nullptr,nullptr,nullptr);
      auto generation=p.generation();p.tick(5001,5000);
      check(p.generation()==generation&&!p.snapshot()["schedule"].is_null(),"Schedule must remain pending on failed durable write");
      sqlite3_exec(db,"ROLLBACK",nullptr,nullptr,nullptr);sqlite3_close(db);
      p.tick(10002,10000);
      check(p.generation()==generation+1&&p.snapshot()["scheduleOutcome"]["outcome"]=="applied","Due schedule must retry after storage recovers");
      check(!p.visible(),"Applied schedule must not claim presentation");
      auto applied_id = p.desired()->id;
      p.packaged_engine({{"identity", "packaged"}});
      p.engine_status(applied_id, {{"imageSHA256", "previous"}});
      p.command("playback.replace", {{"playlist", playlist()}});
      check(p.snapshot()["engine"].value("identity", "") == "packaged", "New selection retained previous engine pin");
      auto retained=p.state();
      try{p.command("playback.replace",{{"playlist",playlist()},{"playlistUrl",std::string(17000,'x')}});throw std::runtime_error("Oversized URL accepted");}
      catch(const ff::Error& error){check(error.code=="playlist_invalid","Wrong URL admission error");}
      check(p.state()==retained,"Invalid URL must not enter durable state");
      p.command("playback.pause",{{"paused",true}});
    }
    {
      ff::Store store(path);ff::Playback p(store);p.tick(1000000,20000);
      auto id=p.desired()->id;p.committed(id);p.presented(id);
      p.tick(1001000,21000);p.command("playback.pause",{{"paused",false}});p.tick(1002001,22001);
      check(p.desired()->index==1,"Restarted pause must not add system uptime to the slot timer");
      p.command("playback.pause",{{"paused",true}});p.tick(1004000,24000);
      id=p.desired()->id;p.committed(id);p.presented(id);p.tick(1005000,25000);
      p.command("playback.pause",{{"paused",false}});p.tick(1006001,26001);
      check(p.desired()->index==0,"Presentation during pause must start timing at that presentation");
      auto permissive=ff::Json{{"version",1},{"showMatureContent",true},{"strictPersonal",false},{"blockUnratedCurated",false}};
      auto restrictive=permissive;restrictive["showMatureContent"]=false;
      p.command("policy.set",permissive);
      p.command("playback.replace",{{"playlist",playlist("mature")},{"context","curated"}});
      id=p.desired()->id;p.committed(id);p.presented(id);
      p.command("playback.replace",{{"playlist",playlist()},{"context","personal"}});
      check(p.visible()->id==id,"Test requires outgoing curated frame during incoming personal load");
      auto incoming=p.desired()->id;p.command("policy.set",restrictive);
      check(!p.visible()&&p.desired()->id==incoming,"Policy must retire blocked outgoing frame using its own context");
      p.command("policy.set",permissive);p.command("playback.replace",{{"playlist",playlist("mature")}});
      id=p.desired()->id;p.committed(id);p.presented(id);p.command("policy.set",restrictive);
      check(!p.desired()&&!p.visible(),"All blocked content must leave no retained presentation");
      p.command("policy.set",permissive);
      check(p.desired().has_value()&&!p.visible(),"Relaxed policy must restore retained active intent without inventing presentation");
    }

    {
      ff::Store store(path+".first-timed-refresh");
      ff::Playback p(store);
      p.tick(1, 1000);
      p.system_boot("first");
      ff::Json policy = {{"version",1},{"showMatureContent",true},{"strictPersonal",false},{"blockUnratedCurated",false}};
      p.command("policy.set", policy);
      auto source = playlist();
      source["items"][0]["display"] = {{"loop", false}};
      const std::string url = "https://example.org/exhibition";
      p.command("playback.replace", {{"playlist",source},{"playlistUrl",url},{"boot",true}});
      const auto current = p.desired()->id;
      p.committed(current);
      p.presented(current);
      const auto replacement = source["items"][1];
      auto relabeled = source["items"][0];
      relabeled["contentRating"] = "mature";
      const ff::Json refresh = {{"items",{replacement, relabeled}}, {"activationTimes",{60000, 70000}}};
      p.command("playback.update", refresh);
      p.tick(2001, 3000);
      check(p.desired()->id == current, "Slot duration must not start a future work early");
      p.ended(current);
      p.tick(2002, 3001);
      check(p.desired()->id == current, "End of media must not start a future work early");
      try {
        p.command("playback.select", {{"index", 1}});
        throw std::runtime_error("Future work selected through the old list");
      } catch (const ff::Error& error) {
        check(error.code == "invalid_params", "Wrong error for ineligible slot");
      }
      p.system_boot("next");
      check(p.snapshot()["presentedOwnsCast"] == true, "Restoring the same source must retain refresh ownership");
      p.command("playback.update", refresh);
      p.tick(59000, 59999);
      check(p.visible() && p.visible()->id == current && p.desired()->id == current,
            "First timed refresh must keep the outgoing work until activation");
      check(p.state()["cohortPlan"]["playlistUrl"] == url &&
            p.state()["boot"]["activationTimes"] == refresh["activationTimes"],
            "First timed refresh must preserve the source and refresh boot intent");
      policy["showMatureContent"] = false;
      p.command("policy.set", policy);
      check(!p.desired() && !p.visible(), "Policy must retire the outgoing slot without selecting a future work");
      p.tick(59001, 60000);
      check(p.desired() && p.desired()->item == replacement, "First scheduled work must activate on time");
      p.command("playback.stop", ff::Json::object());
      try {
        p.command("playback.update", refresh);
        throw std::runtime_error("Refresh revived a stopped source");
      } catch (const ff::Error& error) {
        check(error.code == "stale_generation", "Stopped source must reject timed refreshes");
      }
    }
    {
      ff::Store store(path+".mixed-refresh"); ff::Playback p(store);
      p.tick(1, 1000);
      auto source = playlist();
      p.command("playback.replace", {{"playlist",source}});
      auto current = p.desired()->id; p.committed(current); p.presented(current);
      auto eligible = source["items"][0]; eligible["source"] = "https://example.org/c";
      const auto future = source["items"][1];
      p.command("playback.update", {{"items",{eligible,future}},{"activationTimes",{nullptr,60000}}});
      try {
        p.command("playback.select", {{"index",1}});
        throw std::runtime_error("Future work selected through a deferred mixed cohort");
      } catch (const ff::Error& error) { check(error.code == "invalid_params", "Wrong deferred selection error"); }
      p.tick(1002, 2001);
      check(p.desired() && p.desired()->item == eligible, "Slot boundary must apply the already eligible work");
    }
    {
      ff::Store store(path+".future-refresh");ff::Playback p(store);
      p.tick(1,3000);
      p.system_boot("first");
      auto source=playlist();
      p.command("playback.replace",{{"playlist",source},{"activationTimes",{2000,3000}},{"boot",true}});
      auto current=p.desired()->id;p.committed(current);p.presented(current);
      auto replacement=source["items"][0];replacement["source"]="https://example.org/new";
      ff::Json refresh={{"items",{replacement}},{"activationTimes",{5000}}};
      auto receipt=p.command("playback.update",refresh);
      check(receipt["updateId"]==p.state()["updateOutcome"]["id"] && p.state()["updateOutcome"]["outcome"]=="applied","Future refresh has a durable terminal outcome");
      check(p.visible()&&p.visible()->id==current,"Future refresh preserves the outgoing work until activation");
      refresh["retireBlockedCurrent"]=true;
      p.command("playback.update",refresh);
      check(!p.visible()&&!p.desired(),"Future refresh retires blocked outgoing work immediately");
      check(p.snapshot()["waitingSource"]["playlist"]["items"]==refresh["items"],"Retired plan exposes a durable refresh source");
      check(p.state()["boot"]["playlist"]["items"]==refresh["items"] &&
            p.state()["boot"]["activationTimes"]==refresh["activationTimes"],"Boot retains the refreshed full plan");
      p.system_boot("second");
      check(!p.desired(),"Boot before activation cannot restore retired artwork");
      p.tick(2,5000);
      check(p.desired()&&p.desired()->item==replacement,"Only the replacement activates after boot");
      current=p.desired()->id;p.committed(current);p.presented(current);
      replacement["contentRating"]="mature";
      p.command("playback.update",{{"items",{replacement}},{"activationTimes",{7000}}});
      check(!p.visible()&&!p.desired(),"A future refresh retires a work blocked by its new labels");
      replacement["contentRating"]="general";
      p.command("playback.update",{{"items",{replacement}},{"activationTimes",{4500}}});
      check(p.desired()&&p.desired()->item==replacement,"A corrected activation time resumes an eligible timed work");
      p.command("playback.update",{{"items",{replacement}},{"activationTimes",{8000}},{"retireBlockedCurrent",true}});
      p.command("playback.update",{{"items",{replacement}}});
      check(p.desired()&&p.desired()->item==replacement&&p.state()["cohortPlan"].is_null(),
            "Removing the final activation time resumes retired playback");
      check(!p.state()["boot"].contains("activationTimes"),"Untimed refresh removes obsolete boot activation times");
    }
    {
      ff::Store store(path);ff::Playback p(store);p.tick(1,1000);
      p.command("playback.replace",{{"playlist",playlist()}});auto old=p.desired()->id;
      p.command("schedule.set",{{"playlist",playlist()},{"atMs",1500}});
      auto planned=playlist();planned["items"][0]["source"]="https://example.org/morning";planned["items"][1]["source"]="https://example.org/evening";
      p.command("playback.replace",{{"playlist",planned},{"activationTimes",{2000,3000}},{"boot",true}});
      check(p.desired()->id==old,"Future-only cohort must retain current artwork");
      check(p.state()["schedule"].is_null(),"Deferred cast must cancel the superseded one-shot schedule");
      p.tick(2,1500);check(p.desired()->id==old,"Superseded schedule must not erase the new plan");
      p.command("schedule.set",{{"playlist",playlist()},{"atMs",4000}});
      p.tick(2,2000);check(p.desired()->item["source"]=="https://example.org/morning","Host must execute timed cohort");
      check(!p.state()["schedule"].is_null(),"Cohort progression must retain a later accepted scheduled cast");
      check(p.state()["boot"]["playlist"]==planned,"Boot intent must retain complete source");
    }
    {
      ff::Store store(path);ff::Playback p(store);p.tick(1,3000);
      check(p.desired()->item["source"]=="https://example.org/evening","Host restart must restore and advance schedule without controld");
      p.tick(2,4000);check(p.desired()->item["source"]=="https://example.org/a","Later scheduled cast must execute after cohort progression and restart");
      auto timed=playlist();timed["items"][0]["source"]="https://example.org/morning";timed["items"][1]["source"]="https://example.org/evening";
      p.command("playback.replace",{{"playlist",timed},{"activationTimes",{2000,3000}}});
      auto beforeHold=p.state();
      p.command("health.set",{{"thermal",true}});check(!p.desired()&&!p.visible(),"Thermal hold must release artwork backends");
      check(p.state()==beforeHold,"Thermal protection must preserve durable playback intent");
      p.command("health.set",{{"thermal",false}});check(p.desired().has_value(),"Cooling must restore current intent");
      auto generation=p.generation();auto current=p.desired()->id;
      p.command("playback.sleep",{{"sleeping",false}});
      check(p.desired()->id==current,"Replaying awake intent must preserve the artwork occurrence");
      p.command("playback.replace",{{"playlist",playlist()},{"onlyIfNoPlaylist",true}});
      check(p.generation()==generation&&p.desired()->id==current,"Conditional fallback must not reset recovered playback");
      auto full=p.state()["cohortPlan"]["playlist"];
      auto url="https://example.org/timed-playlist";
      p.command("playback.replace",{{"playlist",full},{"playlistUrl",url},{"activationTimes",{2000,3000}}});
      auto retained=p.desired()->id;
      p.command("playback.update",{{"items",full["items"]},{"activationTimes",{2000,3000}}});
      check(p.desired()->id==retained&&p.snapshot()["playlistUrl"]==url,"Timed refresh must retain occurrence and original URL");
      p.command("schedule.set",{{"playlist",full},{"index",1},{"activationTimes",{2000,3000}},{"atMs",3500}});
      p.tick(2,3500);check(p.desired()->item["source"]=="https://example.org/evening","Scheduled cast must resolve cohort at execution");
      p.system_boot("first-boot");auto first=p.desired()->id;
      p.system_boot("first-boot");check(p.desired()->id==first,"Host restart in same OS boot must not recast boot playlist");
      p.command("policy.set",{{"version",1},{"showMatureContent",false},{"strictPersonal",false},{"blockUnratedCurated",false}});
      p.command("boot.set",{{"playlist",playlist("mature")}});
      p.system_boot("second-boot");check(p.snapshot()["scheduleOutcome"]["outcome"]=="failed","Blocked boot intent must leave usable host and explicit failure");
      p.system_boot("second-boot");
      p.command("playback.stop",ff::Json::object());p.tick(2,4000);check(!p.desired(),"Stop cancels timed progression");
    }
    std::filesystem::remove_all(directory);std::cout<<"Scheduling, durable failure and pause recovery checks passed\n";
  }catch(const std::exception& error){std::filesystem::remove_all(directory);std::cerr<<error.what()<<'\n';return 1;}
}
