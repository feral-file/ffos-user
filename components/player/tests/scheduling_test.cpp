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
      ff::Store store(path);ff::Playback p(store);p.tick(1000,1000);
      p.command("playback.replace",{{"playlist",playlist()}});auto id=p.desired()->id;p.committed(id);p.presented(id);
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
    std::filesystem::remove_all(directory);std::cout<<"Scheduling, durable failure and pause recovery checks passed\n";
  }catch(const std::exception& error){std::filesystem::remove_all(directory);std::cerr<<error.what()<<'\n';return 1;}
}
