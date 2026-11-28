#include "ff/playback.hpp"
#include <algorithm>
#include <filesystem>
#include <iostream>
#include <set>

namespace {void check(bool value,const char* reason){if(!value)throw std::runtime_error(reason);}}
int main(){
 auto directory=std::filesystem::temp_directory_path()/("ff-modes-"+ff::uuid());std::filesystem::create_directory(directory);
 try {
  ff::Json playlist={{"items",ff::Json::array()}};
  for(int i=0;i<12;++i)playlist["items"].push_back({{"id",std::to_string(i)},{"source","https://example.org/same"},{"duration",1}});
  std::vector<int> retained;
  {
   ff::Store store((directory/"state.sqlite").string());ff::Playback p(store);p.tick(1000,1000);
   p.command("playback.replace",{{"playlist",playlist},{"index",6}});auto current=p.desired()->id;
   p.committed(current);p.presented(current);
   p.command("playback.mode",{{"loop","none"},{"shuffle",true}});
   retained=p.state()["order"].get<std::vector<int>>();
   check(retained.front()==6&&p.desired()->id==current,"Shuffle anchors current slot without remount");
   auto sorted=retained;std::sort(sorted.begin(),sorted.end());
   for(int i=0;i<12;++i)check(sorted[i]==i,"Shuffle keeps every equal-URL slot once");
  }
  {
   ff::Store store((directory/"state.sqlite").string());ff::Playback p(store);p.tick(1000,1000);
   check(p.state()["order"]==retained&&p.state()["shuffle"]==true,"Restart restores exact shuffle order");
   auto current=p.desired()->id;p.committed(current);p.presented(current);
   std::set<int> seen{p.desired()->index};
   for(int i=1;i<12;++i){
    p.tick(1000+i*1100,1000+i*1100);check(p.desired()->id!=current,"Finite shuffle must reach next slot");
    current=p.desired()->id;p.committed(current);p.presented(current);seen.insert(p.desired()->index);
   }
   check(seen.size()==12,"Finite shuffle must visit every slot before stopping");
   p.tick(15000,15000);check(p.desired()->id==current,"Loop none holds final shuffled slot");
   p.command("playback.mode",{{"shuffle",false}});
   check(p.desired()->id==current,"Disabling shuffle keeps current occurrence");
   auto original=p.state()["order"].get<std::vector<int>>();for(int i=0;i<12;++i)check(original[i]==i,"Disabling shuffle restores document order");
   p.command("playback.mode",{{"shuffle",true},{"loop","one"}});
   p.command("playback.replace",{{"playlist",playlist}});
   check(p.state()["shuffle"]==false&&p.state()["loop"]=="playlist","New cast resets prior cast modes");
  }
  std::filesystem::remove_all(directory);std::cout<<"Playback mode and shuffle restart tests passed\n";return 0;
 }catch(const std::exception& error){std::filesystem::remove_all(directory);std::cerr<<error.what()<<'\n';return 1;}
}
