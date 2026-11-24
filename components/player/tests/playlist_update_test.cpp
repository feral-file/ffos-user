#include "ff/playback.hpp"
#include <filesystem>
#include <iostream>
#include <sqlite3.h>

namespace {
void check(bool value,const char* message){if(!value)throw std::runtime_error(message);}
ff::Json item(std::string id,std::string source="",std::string rating="general"){
  return {{"id",id},{"source","https://example.org/"+(source.empty()?id:source)},
    {"duration",10},{"contentRating",rating}};
}
std::string show(ff::Playback& p){auto id=p.desired()->id;p.committed(id);p.presented(id);return id;}
}
int main(){
 auto directory=std::filesystem::temp_directory_path()/("ff-update-"+ff::uuid());std::filesystem::create_directory(directory);
 auto path=(directory/"state.sqlite").string();
 try {
  const ff::Json original={{"id","playlist"},{"title","Exhibition"},{"signature","old-signature"},
    {"items",ff::Json::array({item("a"),item("twin","same"),item("twin","same"),item("b")})}};
  std::string update_id;
  {
   ff::Store store(path);ff::Playback p(store);p.tick(1000,1000);
   p.command("playback.replace",{{"playlist",original},{"index",2}});auto occurrence=show(p);
   p.command("playback.mode",{{"shuffle",true},{"loop","none"}});auto order=p.state()["order"];
   p.command("playback.update",{{"items",original["items"]}});
   check(p.desired()->id==occurrence&&p.state()["order"]==order,"No-op refresh retains mount and shuffle order");
   auto items=ff::Json::array({item("b"),item("a"),item("twin","same"),item("twin","same")});
   items[3]["title"]="Second twin";
   p.command("boot.set",{{"playlist",original},{"index",2}});
   auto generation=p.generation();p.command("playback.update",{{"items",items}});
   check(p.desired()->id==occurrence&&p.visible()->index==3,"Exact duplicate slots remap by ordinal without remount");
   check(p.visible()->item["title"]=="Second twin"&&p.visible()->generation==generation+1,"Fresh labels and trust generation follow retained occurrence");
   check(!p.visible()->playlist.contains("signature")&&!p.state()["boot"]["playlist"].contains("signature"),"Modified document cannot retain signature");
   check(p.state()["boot"]["playlist"]["items"]==items,"Boot projection follows matching refresh");
   check(p.state()["boot"]["index"]==3,"Boot selection follows its duplicate ordinal");
   check(p.state()["loop"]=="none"&&p.state()["shuffle"]==true,"Refresh retains playback modes");
   auto receipt=p.command("playback.update",{{"items",ff::Json::array({item("replacement")})}});
   check(p.state()["boot"]["index"]==0,"Removed boot selection resets to the first refreshed work");
   check(p.snapshot()["presentedOwnsCast"]==true,"Deferred refresh retains cast ownership across command generations");
   update_id=receipt["updateId"];
   check(receipt["outcome"]=="accepted"&&p.visible()->id==occurrence&&p.desired()->id==occurrence,"Removed current work defers without false presentation");
   check(p.snapshot()["pendingUpdate"]["id"]==update_id&&p.state()["playlist"]["items"]==items,"Current document remains authoritative until boundary");
   p.tick(10999,10999);check(p.desired()->id==occurrence,"Refresh must not reset or prematurely finish slot timer");
  }
  {
   ff::Store store(path);ff::Playback p(store);p.tick(1000,1000);auto occurrence=show(p);
   check(p.snapshot()["pendingUpdate"]["id"]==update_id,"Deferred refresh survives restart");
   sqlite3* db=nullptr;sqlite3_open(path.c_str(),&db);sqlite3_exec(db,"BEGIN IMMEDIATE",nullptr,nullptr,nullptr);
   p.tick(11001,11001);
   check(p.desired()->id==occurrence&&!p.snapshot()["pendingUpdate"].is_null(),"Failed durable write preserves current and pending refresh");
   sqlite3_exec(db,"ROLLBACK",nullptr,nullptr,nullptr);sqlite3_close(db);
   p.tick(16002,16002);
   check(p.desired()->item["id"]=="replacement"&&p.visible()->id==occurrence,"Boundary selects refreshed list while old occurrence remains until commit");
   check(p.snapshot()["pendingUpdate"].is_null()&&p.snapshot()["updateOutcome"]["id"]==update_id,"Deferred operation exposes terminal result");
   show(p);
   p.command("playback.replace",{{"playlist",original},{"index",2}});occurrence=show(p);
   auto items=original["items"];items[2]["contentRating"]="mature";
   p.command("playback.update",{{"items",items}});
   check(!p.visible()&&p.desired()->id!=occurrence&&p.desired()->index==3,"New labels retire blocked second twin immediately and select next allowed slot");
   show(p);auto blocked=ff::Json::array({item("b","changed","mature")});
   p.command("playback.update",{{"items",blocked}});
   check(!p.visible()&&!p.desired()&&p.snapshot()["updateOutcome"]["contentBlocked"]==true,"Blocked replacement source cannot leave old frame visible");
   p.command("policy.set",{{"version",1},{"showMatureContent",true},{"strictPersonal",false},{"blockUnratedCurated",false}});
   check(p.desired()->item==blocked[0],"Relaxing policy restores latest intent");
   show(p);p.command("playback.update",{{"items",ff::Json::array()}});
   check(!p.desired()&&!p.visible()&&p.state()["stopped"]==true,"Empty refresh clears immediately");
   p.command("playback.replace",{{"playlist",original}});occurrence=show(p);
   try{p.command("playback.update",{{"items",original["items"]},{"context","personal"}});throw std::runtime_error("Trust escalation accepted");}
   catch(const ff::Error& e){check(e.code=="invalid_params","Wrong trust escalation error");}
   check(p.desired()->id==occurrence,"Rejected context escalation preserves state");
   p.command("playback.update",{{"items",ff::Json::array({item("removed")})}});
   p.command("playback.update",{{"items",original["items"]}});
   check(p.snapshot()["pendingUpdate"].is_null()&&p.desired()->id==occurrence,"Restoring current document cancels queued replacement");
   p.command("playback.update",{{"items",ff::Json::array({item("removed")})}});
   p.command("playback.stop",ff::Json::object());
   check(p.snapshot()["pendingUpdate"].is_null(),"Explicit stop cancels deferred mutation");
   p.command("policy.set",{{"version",1},{"showMatureContent",false},{"strictPersonal",false},{"blockUnratedCurated",false}});
   p.command("playback.replace",{{"playlist",original}});occurrence=show(p);
   p.command("playback.select",{{"index",3}});
   p.command("playback.update",{{"items",ff::Json::array({item("b")})}});
   check(p.visible()->id==occurrence,"Removed outgoing frame may remain while selected replacement loads");
   p.command("playback.update",{{"items",ff::Json::array({item("b"),item("a","a","mature")})}});
   check(!p.visible(),"Later refresh must retire blocked outgoing frame even after earlier refresh changed the admitted document");
   p.command("policy.set",{{"version",1},{"showMatureContent",true},{"strictPersonal",false},{"blockUnratedCurated",false}});
   p.command("playback.replace",{{"playlist",{{"items",ff::Json::array({item("mature","mature","mature")})}}}});show(p);
   p.command("playback.update",{{"items",ff::Json::array({item("allowed")})}});
   p.command("policy.set",{{"version",1},{"showMatureContent",false},{"strictPersonal",false},{"blockUnratedCurated",false}});
   check(!p.desired()&&!p.visible(),"Tightening policy immediately retires old work");
   p.tick(17000,17000);
   check(p.desired()&&p.desired()->item["id"]=="allowed","Policy retirement must not strand an allowed deferred replacement");
   auto source = item("capsule");
   p.command("playback.replace", {{"playlist", {{"items", ff::Json::array({source})}}}});
   occurrence = show(p);
   source["ffCapsuleId"] = std::string(64, 'a');
   p.command("playback.update", {{"items", ff::Json::array({source})}});
   check(p.desired()->id != occurrence && !p.visible()->item.contains("ffCapsuleId"),
         "A capsule capture must select a new occurrence instead of relabeling live playback");
  }
  {
   ff::Store store(path);
   auto legacy=store.load();
   legacy["boot"]={{"playlist",{{"items",ff::Json::array({item("b")})}}},{"index",1}};
   legacy["systemBootId"]="previous";
   store.save(legacy);
   ff::Playback p(store);
   p.system_boot("next");
   check(p.desired() && p.desired()->item["id"]=="b" && store.load()["boot"]["index"]==0,
         "Boot repairs the stale saved index from a previous build");
  }
  std::filesystem::remove_all(directory);std::cout<<"Playlist refresh identity, policy, persistence and boundary tests passed\n";return 0;
 }catch(const std::exception& error){std::filesystem::remove_all(directory);std::cerr<<error.what()<<'\n';return 1;}
}
