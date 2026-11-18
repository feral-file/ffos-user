#include "ff/input.hpp"
#include <iostream>
#include <vector>

namespace {
void check(bool condition,const char* message){if(!condition)throw std::runtime_error(message);}
}
int main(){
  try {
    std::vector<ff::Json> events;
    ff::InputState input([&](const ff::Json& event){events.push_back(event);});
    input.target("first",{2000,1000,100,50,1800,900});
    check(input.x()==1000&&input.y()==500,"First artwork centers the host pointer once");
    input.dispatch({{"kind","move"},{"x",900},{"y",400}});
    input.flush();
    check(input.x()==900&&input.y()==400,"Host owns output-pixel pointer position");
    check(events.back()["x"]==800&&events.back()["y"]==350,"Artwork coordinates remove host margins");
    input.dispatch({{"kind","key"},{"usage",225},{"down",true}});
    input.dispatch({{"kind","button"},{"button",1},{"down",true},{"clicks",2}});
    check(events.back()["shift"]==true&&events.back()["buttons"]==1,"Drag and modifier state share authority");
    input.dispatch({{"kind","move"},{"dx",50},{"dy",25}});
    input.flush();
    check(events.back()["buttons"]==1&&events.back()["x"]==850,"Relative movement preserves held drag");
    input.target("second",{2000,1000,0,0,2000,1000});
    check(input.x()==950&&input.y()==425,"New occurrence does not reset the visible pointer");
    check(events[events.size()-3]["kind"]=="button"&&events[events.size()-3]["target"]=="first"&&events[events.size()-3]["down"]==false,"Old target receives button release before destruction");
    check(events[events.size()-2]["kind"]=="key"&&events[events.size()-2]["target"]=="first"&&events[events.size()-2]["down"]==false,"Old target receives key release before destruction");
    input.dispatch({{"kind","button"},{"down",true}});
    check(events.back()["x"]==950&&events.back()["y"]==425&&events.back()["target"]=="second","Click matches retained cursor on new work");
    input.dispatch({{"kind","releaseAll"}});
    check(input.buttons()==0,"Disconnect/focus loss releases held input");
    input.target("second",{500,300,10,20,480,260});
    check(input.x()==499&&input.y()==299,"Display resize clamps the authoritative cursor");
    input.dispatch({{"kind","move"},{"x",0},{"y",0}});
    input.flush();
    check(events.back()["outside"]==true,"Matte is outside the artwork input bounds");
    input.dispatch({{"kind","key"},{"usage",4},{"down",true}});
    input.dispatch({{"kind","key"},{"usage",4},{"down",true}});
    check(events.back()["repeat"]==true,"Repeated physical key is distinct from text");
    input.dispatch({{"kind","text"},{"text","é猫"}});
    check(events.back()["text"]=="é猫","Text is preserved independently of layout/physical key");
    auto before_moves=events.size();
    for(int n=0;n<100;++n)input.dispatch({{"kind","move"},{"dx",1},{"dy",1}});
    check(events.size()==before_moves,"Contiguous moves do not flood the renderer queue");
    input.dispatch({{"kind","button"},{"down",true}});
    check(events.size()==before_moves+2&&events[before_moves]["kind"]=="move"&&
      events.back()["kind"]=="button"&&events.back()["x"]==90&&events.back()["y"]==80,
      "Coalescing preserves final position and flushes before button transition");
    input.dispatch({{"kind","move"},{"x",0},{"y",0}});
    input.dispatch({{"kind","move"},{"dx",20},{"dy",10},{"referenceWidth",1000}});input.flush();
    check(input.x()==10&&input.y()==5,"Host scales phone deltas using current output geometry");
    input.target("second",{1000,500,0,0,1000,500});
    input.dispatch({{"kind","move"},{"dx",20},{"dy",10},{"referenceWidth",1000}});input.flush();
    check(input.x()==30&&input.y()==15,"Resize updates relative movement without stale client dimensions");
    input.target("",{500,300,0,0,500,300});auto count=events.size();
    input.dispatch({{"kind","move"},{"x",100}});
    check(events.size()==count,"Absent target receives no new input");
    check(ff::physical_key(4).virtual_key==65&&ff::physical_key(4).native_key==38,"KeyA maps to Chromium VK_A and Linux XKB");
    check(ff::physical_key(80).virtual_key==37&&ff::physical_key(80).native_key==113,"ArrowLeft does not use SDL key-symbol encoding");
    bool rejected=false;try{ff::physical_key(255);}catch(const ff::Error&){rejected=true;}
    check(rejected,"Unsupported physical usage fails explicitly");
    std::cout<<"Host pointer, target lifecycle, modifiers, keyboard and text checks passed\n";
  }catch(const std::exception& e){std::cerr<<e.what()<<'\n';return 1;}
}
