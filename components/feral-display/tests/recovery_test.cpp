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
    auto expect_no_artwork=[&]{
      auto before=p.snapshot();bool refused=false;
      try{p.command("playback.refresh",ff::Json::object());}catch(const ff::Error& e){refused=e.code=="no_artwork";}
      check(refused&&p.snapshot()==before,"Refresh without artwork must reject without changing state");
    };
    expect_no_artwork();
    p.tick(0,0);p.command("playback.replace",{{"playlist",playlist()}});
    auto covered=p.desired()->id;p.committed(covered);p.frame_ready(covered);
    p.command("overlay.set",{{"owner","setup"},{"state","joining"}});p.tick(120000,120000);
    check(p.desired()->id==covered&&p.desired()->stage=="committed"&&!p.visible(),"Ready artwork covered by setup must not time out or claim presentation");
    check(p.state()["history"].empty(),"Covered artwork must not enter history");
    check(p.media_paused(covered),"Incoming media must wait for uncovered presentation");
    p.command("overlay.set",{{"owner","setup"},{"state","ready"}});p.presented(covered);
    check(p.visible()&&p.state()["history"].size()==1,"Uncovering captured artwork must permit presentation");
    check(!p.media_paused(covered),"Presentation must release initial media hold");
    auto short_media=playlist();short_media["items"][0]["display"]={{"loop",false}};
    short_media["items"].push_back({{"source","https://example.org/next"},{"duration",0}});
    p.command("playback.replace",{{"playlist",short_media}});auto early=p.desired()->id;
    p.committed(early);p.frame_ready(early);p.ended(early);p.tick(120001,120001);
    check(p.desired()->id==early,"Early EOS must wait for first uncovered presentation");
    p.presented(early);p.tick(120002,120002);
    check(p.desired()->id!=early&&p.desired()->index==1,"Retained EOS must progress after presentation");
    auto next=p.desired()->id;p.committed(next);p.presented(next);p.ended(early);p.tick(120003,120003);
    check(p.desired()->id==next,"Retired EOS must not advance the replacement");
    p.command("playback.replace",{{"playlist",playlist()}});covered=p.desired()->id;p.committed(covered);p.tick(181001,181001);
    check(p.desired()->stage=="failed","Navigation without captured frame must still time out");
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
    auto held=playlist();held["items"][0]["display"]={{"autoplay",false}};
    p.command("playback.replace",{{"playlist",held}});check(p.media_paused(),"Fixture must begin on autoplay hold");
    p.command("playback.pause",{{"paused",false}});check(!p.media_paused(),"Resume must release autoplay hold");
    p.failed(p.desired()->id,"Temporary decoder failure");p.tick(now+201000,now+201000);
    check(!p.media_paused(),"Automatic recovery must preserve explicit resume of autoplay-disabled media");
    p.command("playback.sleep",{{"sleeping",true}});expect_no_artwork();
    p.command("playback.sleep",{{"sleeping",false}});p.command("playback.stop",ff::Json::object());expect_no_artwork();
    auto mixed=playlist();mixed["items"].push_back({{"source","https://example.org/healthy"},{"duration",0}});
    p.command("playback.replace",{{"playlist",mixed}});
    int64_t clock=now+201000;
    for(int item=0;item<2;++item){
      check(p.desired()->index==item,"An exhausted work must yield to a later work");
      for(int attempt=0;attempt<=3;++attempt){
        p.failed(p.desired()->id,"Broken source");
        clock+=8000;p.tick(clock,clock);
      }
    }
    auto last=p.desired()->id;p.tick(clock+60000,clock+60000);
    check(p.desired()->id==last && p.snapshot()["recovery"]["exhausted"]==true,"An all-broken list must stop");
    auto added=mixed["items"];added.push_back({{"source","https://example.org/added"},{"duration",0}});
    p.command("playback.update",{{"items",added}});p.tick(clock+61000,clock+61000);
    check(p.desired()->index==2,"Refreshed list must release exhausted selection");
    clock+=61000;
    for(int attempt=0;attempt<=3;++attempt){
      p.failed(p.desired()->id,"Unavailable");clock+=8000;
      if(attempt<3)p.tick(clock,clock);
    }
    p.command("playback.update",{{"items",ff::Json::array({{{"source","https://example.org/new"}}})}});
    p.tick(clock,clock);
    check(p.desired()->item["source"]=="https://example.org/new","Refresh can replace an exhausted occurrence");
    p.committed(p.desired()->id);p.presented(p.desired()->id);
    p.command("connectivity.set",{{"online",false}});
    p.command("overlay.set",{{"owner","setup"},{"state","connecting"}});
    p.command("playback.replace",{{"playlist",mixed}});
    check(p.overlay().is_null(),"An existing presentation must let the offline advisory yield");
    for(int attempt=0;attempt<=3;++attempt){
      p.failed(p.desired()->id,"Offline cache miss");clock+=8000;p.tick(clock,clock);
    }
    check(p.desired()->index==1,"A yielded advisory must allow failed offline artwork to advance");
    std::filesystem::remove_all(directory);std::cout<<"Bounded renderer recovery checks passed\n";
  }catch(const std::exception& error){std::filesystem::remove_all(directory);std::cerr<<error.what()<<'\n';return 1;}
}
