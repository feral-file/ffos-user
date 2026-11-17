#include "ff/playback.hpp"
#include <filesystem>
#include <iostream>

namespace {
void check(bool condition,const char* reason){if(!condition)throw std::runtime_error(reason);}
ff::Json playlist(){return {{"items",{{{"source","https://example.org/work"},{"duration",0}}}}};}
}
int main(){
  auto directory=std::filesystem::temp_directory_path()/("ff-recovery-test-"+ff::uuid());
  std::filesystem::create_directory(directory);
  try{
    ff::Store store((directory/"state.sqlite").string());ff::Playback p(store);
    int releases=0;p.set_input_handler([&](const ff::Json& e){if(e["kind"]=="releaseAll")++releases;});
    p.tick(1000,1000);p.command("playback.replace",{{"playlist",playlist()}});
    const auto generation=p.generation();const auto instance=p.instance();
    auto failed=p.desired()->id;p.committed(failed);p.presented(failed);
    p.failed(failed,"Decoder terminated");auto revision=p.revision();
    p.failed(failed,"Duplicate callback");check(p.revision()==revision,"Duplicate failure must not consume retries");
    check(!p.visible()&&releases>0,"Failed target must release input and presentation");
    int64_t now=1000;
    for(int attempt=1;attempt<=3;++attempt){
      const int delay=1000<<(attempt-1);
      p.tick(now+delay-1,now+delay-1);check(p.desired()->id==failed,"Recovery must respect backoff");
      now+=delay;p.tick(now,now);
      auto current=p.desired()->id;
      check(current!=failed&&p.desired()->stage=="loading","Retry must create a distinct loading occurrence");
      check(p.instance()==instance&&p.generation()==generation,"Renderer recovery must retain host and playlist generation");
      p.committed(failed);p.presented(failed);p.failed(failed,"Late failure");
      check(p.desired()->id==current&&p.desired()->stage=="loading"&&!p.visible(),"Late callbacks must not affect replacement");
      // Brief successful frames must not replenish the retry budget.
      p.committed(current);p.presented(current);p.failed(current,"Decoder terminated");failed=current;
    }
    p.tick(now+120000,now+120000);
    check(p.desired()->id==failed&&p.desired()->stage=="failed","Repeated failures must stop after three retries");
    check(p.snapshot()["recovery"]["exhausted"]==true,"Exhaustion must reach protocol clients");
    p.command("playback.refresh",ff::Json::object());auto manual=p.desired()->id;
    check(manual!=failed&&p.snapshot()["recovery"]["attempt"]==0,"Explicit refresh must begin a new recovery budget");
    p.failed(manual,"Temporary load failure");p.command("playback.sleep",{{"sleeping",true}});
    p.tick(now+130000,now+130000);check(!p.desired(),"Sleep must cancel pending renderer recovery");
    p.command("playback.sleep",{{"sleeping",false}});manual=p.desired()->id;p.failed(manual,"Temporary failure");
    p.tick(now+131000,now+131000);manual=p.desired()->id;p.committed(manual);p.presented(manual);
    p.tick(now+191000,now+191000);
    check(p.snapshot()["recovery"]["attempt"]==0,"Sixty seconds of presentation must restore the recovery budget");
    p.failed(manual,"Later independent failure");p.command("playback.stop",ff::Json::object());
    p.tick(now+200000,now+200000);check(!p.desired(),"Stop must cancel pending recovery");
    p.command("playback.replace",{{"playlist",playlist()}});auto outgoing=p.desired()->id;
    p.committed(outgoing);p.presented(outgoing);
    p.command("playback.replace",{{"playlist",playlist()}});auto incoming=p.desired()->id;
    auto before_release=releases;p.failed(outgoing,"Outgoing renderer terminated");
    check(!p.visible()&&releases>before_release,"Failed outgoing artwork must retire and release its input");
    check(p.desired()->id==incoming&&p.desired()->stage=="loading","Outgoing failure must not change incoming selection");
    check(p.snapshot()["presentationFailure"]["occurrence"]==outgoing,"Outgoing failure must remain observable under its own identity");
    check(p.snapshot()["recovery"]["pending"]==false&&p.snapshot()["recovery"]["attempt"]==0,"Outgoing failure must not charge incoming recovery budget");
    revision=p.revision();p.failed(outgoing,"Duplicate");check(p.revision()==revision,"Retired outgoing callback must be ignored");
    p.committed(incoming);p.presented(incoming);
    check(p.snapshot()["presentationFailure"].is_null(),"New presentation must clear outgoing failure");
    auto held=playlist();held["items"][0]["display"]={{"autoPlay",false}};
    p.command("playback.replace",{{"playlist",held}});check(p.media_paused(),"Fixture must begin on autoplay hold");
    p.command("playback.pause",{{"paused",false}});check(!p.media_paused(),"Resume must release autoplay hold");
    p.failed(p.desired()->id,"Temporary decoder failure");p.tick(now+201000,now+201000);
    check(!p.media_paused(),"Automatic recovery must preserve explicit resume of autoplay-disabled media");
    std::filesystem::remove_all(directory);std::cout<<"Bounded renderer recovery checks passed\n";
  }catch(const std::exception& error){std::filesystem::remove_all(directory);std::cerr<<error.what()<<'\n';return 1;}
}
