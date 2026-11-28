#include <mpv/client.h>
#include <chrono>
#include <iostream>
#include <stdexcept>
#include <string>

void check(bool ok,const char* message){if(!ok)throw std::runtime_error(message);}
void set(mpv_handle* player,const char* key,const char* value){check(mpv_set_option_string(player,key,value)>=0,key);}
int main(int argc,char** argv){
  mpv_handle* player=nullptr;
  try{
    check(argc==2,"Fixture path required");player=mpv_create();check(player,"Create libmpv");
    set(player,"config","no");set(player,"vo","null");set(player,"ao","null");
    set(player,"hwdec","no");set(player,"pause","yes");set(player,"idle","yes");
    check(mpv_initialize(player)>=0,"Initialize libmpv");
    const char* load[]={"loadfile",argv[1],nullptr};check(mpv_command(player,load)>=0,"Load H.264/AAC fixture");
    bool loaded=false,seek=false,ended=false;auto deadline=std::chrono::steady_clock::now()+std::chrono::seconds(15);
    while(std::chrono::steady_clock::now()<deadline&&!ended){
      auto event=mpv_wait_event(player,0.1);
      if(event->event_id==MPV_EVENT_FILE_LOADED){
        loaded=true;char* codec=mpv_get_property_string(player,"video-codec");check(codec&&std::string(codec).find("H.264")!=std::string::npos,"H.264 decoder selected");mpv_free(codec);
        char* audio=mpv_get_property_string(player,"audio-codec-name");check(audio&&std::string(audio)=="aac","AAC decoder selected");mpv_free(audio);
        int paused=0;check(mpv_get_property(player,"pause",MPV_FORMAT_FLAG,&paused)>=0&&paused,"Initial pause preserved");
        const char* move[]={"seek","1.5","absolute+exact",nullptr};check(mpv_command(player,move)>=0,"Seek accepted");
        paused=0;check(mpv_set_property(player,"pause",MPV_FORMAT_FLAG,&paused)>=0,"Resume accepted");
      }
      if(event->event_id==MPV_EVENT_SEEK)seek=true;
      if(event->event_id==MPV_EVENT_END_FILE){auto end=static_cast<mpv_event_end_file*>(event->data);check(end->reason==MPV_END_FILE_REASON_EOF,"Clean media EOF");ended=true;}
    }
    check(loaded&&seek&&ended,"Load, seek and EOF all observed");
    mpv_terminate_destroy(player);std::cout<<"H.264/AAC decode, initial pause, seek, resume and EOF passed (null outputs; no visual/hardware claim)\n";
  }catch(const std::exception& error){if(player)mpv_terminate_destroy(player);std::cerr<<error.what()<<'\n';return 1;}
}
