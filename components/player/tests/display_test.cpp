#include "ff/playback.hpp"
#include "ff/display.hpp"
#include "ff/surface.hpp"
#include <filesystem>
#include <iostream>

namespace {
void check(bool ok,const char* why){if(!ok)throw std::runtime_error(why);}
template<class F> void rejects(F fn,const char* code){try{fn();}catch(const ff::Error& e){check(e.code==code,"Wrong display rejection");return;}throw std::runtime_error("Expected display rejection");}
ff::Json playlist(){return {{"items",{{{"source","https://example.org/same"},{"duration",0}},{{"source","https://example.org/same"},{"duration",0}}}}};}
}
int main(){
 auto dir=std::filesystem::temp_directory_path()/("ff-display-"+ff::uuid());std::filesystem::create_directory(dir);
 try{
  ff::FramePair frames;frames.capture.texture=10;frames.capture.display_revision=1;frames.captured();frames.submitted();
  frames.capture.texture=11;frames.capture.display_revision=2;frames.captured();
  check(frames.shown.texture==10&&frames.shown.display_revision==1,"Pending R2 must retain the composed R1 texture when R3 supersedes it");
  frames.capture.display_revision=3;frames.captured();frames.submitted();
  check(frames.shown.texture==11&&frames.shown.display_revision==3&&frames.capture.texture==10,"Submission must swap texture ownership without a copy");
  for(auto [literal,expected]:{std::pair{"#fff","#FFFFFF"},std::pair{"white","#FFFFFF"},std::pair{"gray","#808080"},std::pair{"#f008","#880000"},std::pair{"rgba(255, 0, 0, 0.5)","#800000"},std::pair{"rgb(100% 0% 0% / 50%)","#800000"},std::pair{"hsl(120deg, 100%, 50%)","#00FF00"},std::pair{"transparent","#000000"}})
   check(ff::display_color(literal)==expected,"CSS color must resolve identically for native and bundled/media backends");
  rejects([]{ff::display_color("var(--artist-color)");},"playlist_invalid");
  check(ff::hard_pixel_edges({{"scaling","fill"}},400,200,500,500),"Cover enlargement uses the larger axis");
  check(!ff::hard_pixel_edges({{"scaling","fit"}},400,200,500,500),"Small enlargement must stay smooth");
  check(ff::hard_pixel_edges({{"imageRendering","pixelated"}},400,200,100,100),"Authored pixelation wins on reduction");
  auto landscape=ff::display_bounds({{"margin","10%"}},1920,1080);
  check(landscape.x==192&&landscape.y==108&&landscape.width==1536&&landscape.height==864,"Percentage matte must use each viewport axis");
  auto portrait=ff::display_bounds({{"margin","10%"}},1080,1920);
  check(portrait.x==108&&portrait.y==192,"Rotating must swap percentage insets");
  auto pixels=ff::display_bounds({{"margin",100}},1920,1080);
  check(pixels.x==100&&pixels.y==100,"Pixel matte must use equal insets");
  auto css=ff::display_bounds({{"margin","1rem 2vw 3px 4vh"}},1000,500);
  check(css.x==20&&css.y==16&&css.width==960&&css.height==481,"CSS padding follows top right bottom left");
  auto large=ff::display_bounds({{"margin","90%"}},1920,1080);
  check(large.width>0&&large.height>0,"Extreme matte must retain a bounded renderable viewport");
  {
   ff::Store store((dir/"state.sqlite").string());ff::Playback p(store);
   auto colors=playlist();colors["items"][0]["display"]={{"background","white"}};
   p.command("playback.replace",{{"playlist",colors}});
   check(p.desired()->display["background"]=="white","Authored background must remain intact in product state");
   auto admitted=p.snapshot();colors["items"][0]["display"]["background"]="var(--color)";
   rejects([&]{p.command("playback.replace",{{"playlist",colors}});},"playlist_invalid");
   check(p.snapshot()==admitted,"Unsupported contextual color must reject before changing playback state");
   p.command("settings.set",{{"margin",45},{"background","#123456"}});
   p.command("playback.replace",{{"playlist",playlist()}});auto id=p.desired()->id;
   check(p.desired()->display["margin"]==45&&p.desired()->display["background"]=="#123456","Saved defaults must reach effective presentation");
   p.committed(id);p.presented(id);p.composed(id,p.desired()->display_revision);
   auto stage=p.desired()->display;stage["margin"]=20;
   p.composed(id,p.desired()->display_revision,&stage);
   check(p.composition_pending()&&p.snapshot()["composition"]["display"]["margin"]==20,"Transition status must report the actual old stage under the new occurrence");
   rejects([&]{p.command("presentation.set",{{"occurrence",id},{"display",{{"margin",25}}}});},"stale_occurrence");
   p.composed(id,p.desired()->display_revision);
   check(!p.composition_pending()&&p.snapshot()["composition"]["display"]["margin"]==45,"Final stage needs a fresh composition even with the same target revision");
   auto before=p.snapshot()["composition"];
   auto receipt=p.command("presentation.set",{{"occurrence",id},{"display",{{"margin","10%"},{"background","#ABCDEF"}}}});
   check(receipt["acceptedCompositionRevision"]==before["revision"],"Acceptance must report the previous composition floor");
   check(p.snapshot()["composition"]==before,"Acceptance cannot fabricate a host composition");
   auto revision=p.desired()->display_revision;
   p.composed(id,revision-1);check(p.snapshot()["composition"]==before,"Stale renderer configuration cannot acknowledge newer settings");
   p.composed(id,revision);auto after=p.snapshot()["composition"];
   check(after["revision"]>before["revision"]&&after["display"]["margin"]=="10%","Current configuration must commit effective settings");
   receipt=p.command("presentation.set",{{"occurrence",id},{"display",{{"margin","10%"}}}});
   p.composed(id,p.desired()->display_revision);
   check(p.snapshot()["composition"]["revision"]>receipt["acceptedCompositionRevision"],"No-op accepted preview still needs causal completion");
   p.command("playback.select",{{"index",1}});
   rejects([&]{p.command("presentation.set",{{"occurrence",id},{"display",{{"scaling","fill"}}}});},"stale_occurrence");
   check(p.desired()->display["margin"]==45,"Adjacent identical URL must not inherit preview");
   auto mapped=playlist();
   mapped["defaults"]["display"]["userOverrides"]={{"duration",false},{"scaling",true}};
   mapped["items"][0]["display"]={{"scaling","fit"},{"userOverrides",{{"scaling",false}}}};
   p.command("settings.set",{{"framing","fill"}});
   p.command("playback.replace",{{"playlist",mapped}});id=p.desired()->id;p.committed(id);p.presented(id);
   check(p.desired()->display["scaling"]=="fit" && p.desired()->display["overridePermissions"]["duration"]==false,
         "Per-field locks must merge across layers and retain the artwork's scaling");
   rejects([&]{p.command("presentation.set",{{"occurrence",id},{"display",{{"scaling","fill"}}}});},"presentation_locked");
   p.command("presentation.set",{{"occurrence",id},{"display",{{"margin",10}}}});
   auto authored=playlist();authored["items"][0]["display"]={{"margin",12},{"background","#654321"},{"userOverrides",false}};
   p.command("playback.replace",{{"playlist",authored}});id=p.desired()->id;p.committed(id);p.presented(id);
   check(p.desired()->display["margin"]==12,"Authored margin must beat saved fallback");
   rejects([&]{p.command("presentation.set",{{"occurrence",id},{"display",{{"scaling","fill"}}}});},"presentation_locked");
  }
  {ff::Store store((dir/"state.sqlite").string());ff::Playback p(store);check(p.snapshot()["composition"].is_null(),"Restart must not retain composed success");check(p.state()["settings"]["margin"]==45,"Restart must retain saved preferences");}
  std::filesystem::remove_all(dir);std::cout<<"Display ownership, causal composition and viewport geometry checks passed\n";
 }catch(const std::exception& e){std::filesystem::remove_all(dir);std::cerr<<e.what()<<'\n';return 1;}
}
