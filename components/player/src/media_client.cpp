#include "ff/media_client.hpp"
#include "ff/media_ipc.hpp"
#include <algorithm>
#include <drm_fourcc.h>
#include <spawn.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <unistd.h>
#include <csignal>
#include <cerrno>
extern char** environ;

namespace ff {
namespace { std::vector<pid_t> retiring; }
void reap_media_workers(){
  for(auto it=retiring.begin();it!=retiring.end();){
    auto result=waitpid(*it,nullptr,WNOHANG);
    if(result==*it||(result<0&&errno==ECHILD))it=retiring.erase(it);else ++it;
  }
}
MediaClient::MediaClient(Playback& playback,Presentation& presentation,Occurrence occurrence,const std::string& worker)
 :id(occurrence.id),playback_(playback),presentation_(presentation),occurrence_(std::move(occurrence)) {
  int sockets[2];require(socketpair(AF_UNIX,SOCK_SEQPACKET|SOCK_CLOEXEC|SOCK_NONBLOCK,0,sockets)==0,"media_spawn","Cannot create private media channel");
  socket_=sockets[0];posix_spawn_file_actions_t actions;posix_spawn_file_actions_init(&actions);
  posix_spawn_file_actions_addclose(&actions,sockets[0]);posix_spawn_file_actions_adddup2(&actions,sockets[1],3);
  if(sockets[1]!=3)posix_spawn_file_actions_addclose(&actions,sockets[1]);
  char* args[]={const_cast<char*>(worker.c_str()),nullptr};
  auto rc=posix_spawn(&pid_,worker.c_str(),&actions,nullptr,args,environ);
  posix_spawn_file_actions_destroy(&actions);close(sockets[1]);
  if(rc!=0){close(socket_);socket_=-1;throw Error("media_spawn","Cannot launch media worker: "+std::to_string(rc));}
  try{
    auto bounds=presentation_.bounds(occurrence_.display);
    send_media(socket_,{{"method","initialize"},{"source",occurrence_.item["source"]},{"width",bounds.width},{"height",bounds.height},{"display",occurrence_.display},
      {"paused",playback_.media_paused(id)},
      {"volume",playback_.state()["settings"].value("volume",1.0)},{"muted",playback_.state()["settings"].value("muted",false)}});
  }catch(...){kill(pid_,SIGKILL);retiring.push_back(pid_);close(socket_);throw;}
}
MediaClient::~MediaClient(){
  if(socket_>=0)close(socket_);
  // Do not wait for decoder teardown on the product event loop. No durable
  // data exists in the worker. SIGKILL also bounds a stuck third-party codec.
  if(pid_>0){kill(pid_,SIGKILL);retiring.push_back(pid_);}
  presentation_.destroy(surface);
}
void MediaClient::poll(){
  reap_media_workers();
  if(failed_)return;
  try{
    for(int n=0;n<64;++n){
      auto packet=receive_media(socket_);if(!packet)break;
      received_=std::chrono::steady_clock::now();
      const auto& p=packet->body;auto event=string_field(p,"event",32);
      if(event=="loaded"){loaded_=true;playback_.committed(id);}
      else if(event=="frame"){
        CefAcceleratedPaintInfo info{};
        auto format=p.at("format").get<uint32_t>();
        require(format==DRM_FORMAT_ARGB8888||format==DRM_FORMAT_ABGR8888,"media_protocol","Unsupported media frame format");
        info.format=format==DRM_FORMAT_ARGB8888?CEF_COLOR_TYPE_BGRA_8888:CEF_COLOR_TYPE_RGBA_8888;
        info.modifier=p.at("modifier").get<uint64_t>();
        const auto& planes=p.at("planes");require(planes.is_array()&&!planes.empty()&&planes.size()<=4&&planes.size()==packet->descriptors.size(),"media_protocol","Invalid media planes");
        info.plane_count=planes.size();
        for(size_t i=0;i<planes.size();++i){info.planes[i].fd=packet->descriptors[i];info.planes[i].stride=planes[i].at("stride").get<uint32_t>();info.planes[i].offset=planes[i].at("offset").get<uint64_t>();}
        int width=p.at("width").get<int>(),height=p.at("height").get<int>();
        require(width>0&&height>0&&width<=8192&&height<=8192,"media_protocol","Invalid frame size");
        info.extra.coded_size={width,height};info.extra.visible_rect={0,0,width,height};
        presentation_.accelerated(surface,info);
        received_sequence_=p.at("sequence").get<uint64_t>();
        send_media(socket_,{{"method","ack"},{"sequence",p.at("sequence")}});
        // At most one decoded frame per host swap; don't drain and discard
        // future video frames while servicing a burst of decoder messages.
        break;
      }else if(event=="ended")playback_.ended(id);
      else if(event=="error")throw Error("media_backend",string_field(p,"message",1024));
      else if(event=="property")status[string_field(p,"name",64)]=p.at("value");
      else require(event=="initialized"||event=="heartbeat","media_protocol","Unexpected media event");
    }
    ready=loaded_&&surface.frames>0;
    require(std::chrono::steady_clock::now()-received_<std::chrono::seconds(5),"media_unresponsive","Media worker stopped responding for 5 seconds");
    status["workerPid"]=pid_;status["importedFrames"]=surface.frames;
    status["surfaceWidth"]=surface.width;status["surfaceHeight"]=surface.height;
    status["workerResponsive"]=true;
    playback_.media_status(id,status);
    int exit_status=0;auto ended=waitpid(pid_,&exit_status,WNOHANG);
    if(ended==pid_){pid_=-1;throw Error("media_exit","Media worker exited: "+std::to_string(exit_status));}
    controls();
  }catch(const std::exception& error){fail(error.what());}
}
void MediaClient::fail(const std::string& message){
  failed_=true;ready=false;
  if(pid_>0){kill(pid_,SIGKILL);retiring.push_back(pid_);pid_=-1;}
  if(socket_>=0){close(socket_);socket_=-1;}
  playback_.failed(id,message);
}
void MediaClient::controls(){
  if(playback_.desired()&&playback_.desired()->id==id)occurrence_.display=playback_.desired()->display;
  else if(playback_.visible()&&playback_.visible()->id==id)occurrence_.display=playback_.visible()->display;
  auto bounds=presentation_.bounds(occurrence_.display);
  Json value={{"method","controls"},{"paused",playback_.media_paused(id)},
    {"volume",playback_.state()["settings"].value("volume",1.0)},{"muted",playback_.state()["settings"].value("muted",false)},
    {"display",occurrence_.display},{"width",bounds.width},{"height",bounds.height}};
  auto key=value.dump();if(key!=controls_){send_media(socket_,value);controls_=key;}
}
void MediaClient::seek(double seconds){require(!failed_,"media_unavailable","Media worker has failed");send_media(socket_,{{"method","controls"},{"seekSeconds",seconds}});}
void MediaClient::did_present(){
  if(failed_||received_sequence_==presented_sequence_)return;
  try{
    send_media(socket_,{{"method","presented"},{"sequence",received_sequence_}});
    presented_sequence_=received_sequence_;
  }catch(const std::exception& error){fail(error.what());}
}
}
