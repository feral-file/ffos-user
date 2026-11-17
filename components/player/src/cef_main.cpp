#include "ff/cef_client.hpp"
#include "ff/server.hpp"
#include "ff/media_client.hpp"
#include "include/cef_request_context.h"
#include "include/cef_task.h"
#include <chrono>
#include <csignal>
#include <filesystem>
#include <iostream>
#include <map>
#include <sys/stat.h>
#include <exception>

namespace {
volatile std::sig_atomic_t stopping=0;
void stop(int){stopping=1;}
// Run host work as a scheduled UI task. CefDoMessageLoopWork drains until idle:
// a continuous stream of GPU paints can keep it from returning indefinitely.
// Never put socket service, presentation or shutdown deadlines after that drain.
class HostTick final : public CefTask {
 public:
  explicit HostTick(std::function<bool()> step):step_(std::move(step)){}
  void Execute() override {
    if(!step_()){CefQuitMessageLoop();return;}
    if(!CefPostDelayedTask(TID_UI,this,1)){
      std::cerr<<"CEF refused the next host tick; terminating host\n";
      std::_Exit(1);
    }
  }
 private:
  std::function<bool()> step_;
  IMPLEMENT_REFCOUNTING(HostTick);
};
}
// Chromium changes the stack canary when the zygote forks. Its caller must
// use the same annotation as CefExecuteProcess (CEF #3912).
NO_STACK_PROTECTOR int main(int argc,char** argv){
  umask(0077);
  auto command=CefCommandLine::CreateCommandLine();command->InitFromArgv(argc,argv);
  std::string resources=command->GetSwitchValue("renderer-dir");
  if(resources.empty())resources="/usr/share/ff-player-native/renderers";
  auto app=ff::renderer_app();CefMainArgs args(argc,argv);
  int child=CefExecuteProcess(args,app,nullptr);if(child>=0)return child;
  std::signal(SIGTERM,stop);std::signal(SIGINT,stop);std::signal(SIGPIPE,SIG_IGN);
  std::string state_path=command->GetSwitchValue("state-dir"),socket=command->GetSwitchValue("socket");
  auto worker=(std::filesystem::canonical("/proc/self/exe").parent_path()/"media/ff-media-launch").string();
  if(command->HasSwitch("media-worker"))worker=command->GetSwitchValue("media-worker");
  if(state_path.empty()||socket.empty()){std::cerr<<"Required: --state-dir=PATH --socket=PATH; optional --renderer-dir=PATH\n";return 2;}
  bool initialized=false;
  try{
    std::filesystem::create_directories(state_path);
    ff::Store store(state_path+"/state.sqlite");ff::Playback playback(store);ff::Protocol protocol(playback);ff::Server server(socket,protocol);
    CefSettings settings;settings.windowless_rendering_enabled=true;
    CefString(&settings.root_cache_path)=std::filesystem::absolute(state_path+"/cef").string();
    CefString(&settings.log_file)=state_path+"/cef.log";
    // Sandbox stays enabled. No disabling TLS, web security, site isolation or
    // artist response headers. Child processes execute this same binary.
    ff::require(CefInitialize(args,settings,app,nullptr),"cef_initialization","CEF initialization failed");initialized=true;
    // Chromium installs signal handlers during initialization. Our event loop
    // must receive graceful systemd shutdown after that setup has finished.
    std::signal(SIGTERM,stop);std::signal(SIGINT,stop);
    {
      ff::Presentation presentation(command->HasSwitch("diagnostic-cpu"));
      std::map<std::string,CefRefPtr<ff::ArtworkClient>> clients;
      std::map<std::string,std::unique_ptr<ff::MediaClient>> media;
      ff::InputState input_state([&](const ff::Json& event){
        if(auto it=clients.find(event.at("target").get<std::string>());it!=clients.end())it->second->input(event);
      });
      auto sync_input=[&]{
        ff::InputGeometry geometry;geometry.width=presentation.width();geometry.height=presentation.height();
        std::string target;
        if(playback.visible()){
          target=playback.visible()->id;auto bounds=presentation.bounds(playback.visible()->display);
          geometry.left=bounds.x;geometry.top=bounds.y;geometry.view_width=bounds.width;geometry.view_height=bounds.height;
        }
        input_state.target(target,geometry);
        presentation.pointer_position(input_state.x(),input_state.y());
      };
      auto input=[&](const ff::Json& event){
        sync_input();input_state.dispatch(event);
        if(event.value("kind","")=="move"||event.value("kind","")=="button")presentation.pointer(input_state.x(),input_state.y());
      };
      playback.set_input_handler(input);
      playback.set_seek_handler([&](const std::string& id,double seconds){
        auto it=media.find(id);ff::require(it!=media.end(),"unsupported","This artwork has no native media seek control");
        ff::require(it->second->status.contains("seekable")&&it->second->status["seekable"]==true,"unsupported","This media source is not seekable");it->second->seek(seconds);
      });
      std::string displayed;
      auto frame=[&]{
        auto now=std::chrono::steady_clock::now().time_since_epoch();
        playback.tick(std::chrono::duration_cast<std::chrono::milliseconds>(now).count(),ff::wall_ms());server.poll();
        if(!presentation.events([&](const ff::Json& event){try{if(event.value("kind","")=="releaseAll" || (playback.overlay().is_null()&&(!playback.desired()||playback.desired()->stage!="failed")))input(event);}catch(const std::exception& e){std::cerr<<"input: "<<e.what()<<'\n';}})){stopping=1;return;}
        input_state.flush();
        if(auto desired=playback.desired();desired&&desired->stage!="failed"&&!clients.contains(desired->id)&&!media.contains(desired->id)){
          if(ff::native_media(desired->item)){
            try{media.emplace(desired->id,std::make_unique<ff::MediaClient>(playback,presentation,*desired,worker));}
            catch(const std::exception& error){playback.failed(desired->id,error.what());}
          }else{
          try{
          // Retiring CEF views may close asynchronously. Bound the live set
          // even if a browser never delivers its creation/close callback.
          ff::require(clients.size()<8,"renderer_capacity","Previous artwork browsers have not closed");
          auto url=ff::renderer_url(*desired);bool bundled=url=="ff-render://bundle/index.html";
          CefWindowInfo window;window.SetAsWindowless(0);window.shared_texture_enabled=true;window.external_begin_frame_enabled=true;window.runtime_style=CEF_RUNTIME_STYLE_ALLOY;
          CefBrowserSettings browser_settings;browser_settings.windowless_frame_rate=60;browser_settings.background_color=CefColorSetARGB(255,0,0,0);
          // A fresh non-shared request context isolates cookies/cache/storage.
          // CEF may still share renderer/GPU processes: this is not a process guarantee.
          CefRequestContextSettings context_settings;auto context=CefRequestContext::CreateContext(context_settings,nullptr);
          if(bundled)ff::register_renderer_resources(context,resources);
          auto client=CefRefPtr<ff::ArtworkClient>(new ff::ArtworkClient(playback,presentation,*desired,bundled));
          if(CefBrowserHost::CreateBrowser(window,client,url,browser_settings,nullptr,context))clients.emplace(desired->id,client);
          else playback.failed(desired->id,"Cannot create artwork browser");
          }catch(const std::exception& error){playback.failed(desired->id,error.what());}
          }
        }
        for(auto& [id,client]:media)client->poll();
        ff::Surface* surface=nullptr;std::string commit;
        if(auto desired=playback.desired();desired){auto it=clients.find(desired->id);if(it!=clients.end()&&it->second->has_ready_frame()&&desired->stage!="failed"){surface=&it->second->surface;commit=desired->id;}}
        if(auto desired=playback.desired();desired){auto it=media.find(desired->id);if(it!=media.end()&&it->second->ready&&desired->stage!="failed"){surface=&it->second->surface;commit=desired->id;}}
        if(!surface&&playback.visible()){auto it=clients.find(playback.visible()->id);if(it!=clients.end())surface=&it->second->surface;}
        if(!surface&&playback.visible()){auto it=media.find(playback.visible()->id);if(it!=media.end())surface=&it->second->surface;}
        // Presentation identity advances only after a host-owned frame exists.
        // A swap is composition evidence, never proof of artist correctness or HDMI output.
        const auto* drawing = !commit.empty() ? &*playback.desired() : (playback.visible() ? &*playback.visible() : nullptr);
        presentation.draw(surface,drawing,playback);
        if(drawing)if(auto it=media.find(drawing->id);it!=media.end())it->second->did_present();
        if(!commit.empty())playback.presented(commit);
        sync_input();
        for(auto it=clients.begin();it!=clients.end();){
          auto client=it->second;
          bool keep=(playback.desired()&&playback.desired()->stage!="failed"&&playback.desired()->id==it->first)||(playback.visible()&&playback.visible()->id==it->first);
          if(!keep)client->close();else client->update_media();
          if(client->closed)it=clients.erase(it);else ++it;
        }
        for(auto it=media.begin();it!=media.end();){
          bool keep=(playback.desired()&&playback.desired()->stage!="failed"&&playback.desired()->id==it->first)||(playback.visible()&&playback.visible()->id==it->first);
          if(!keep)it=media.erase(it);else ++it;
        }
        ff::reap_media_workers();
      };
      bool closing=false;
      std::exception_ptr frame_error;
      std::chrono::steady_clock::time_point close_deadline;
      auto tick=CefRefPtr<HostTick>(new HostTick([&]{
        if(!stopping){
          try{frame();}
          catch(...){frame_error=std::current_exception();stopping=1;}
        }
        if(!stopping)return true;
        if(!closing){
          closing=true;playback.release_input();media.clear();
          for(auto& [id,client]:clients)client->close();
          close_deadline=std::chrono::steady_clock::now()+std::chrono::seconds(5);
        }
        ff::reap_media_workers();
        for(auto it=clients.begin();it!=clients.end();)if(it->second->closed)it=clients.erase(it);else ++it;
        if(clients.empty())return false;
        if(std::chrono::steady_clock::now()>close_deadline){std::cerr<<"CEF shutdown exceeded 5 seconds; terminating host\n";std::_Exit(1);}
        return true;
      }));
      ff::require(CefPostTask(TID_UI,tick),"cef_initialization","Cannot schedule host work");
      CefRunMessageLoop();
      std::cerr<<"presentation_metrics "<<presentation.metrics().dump()<<'\n';
      if(frame_error)std::rethrow_exception(frame_error);
    }
    CefShutdown();return 0;
  }catch(const std::exception& e){std::cerr<<"ff-player-native: "<<e.what()<<'\n';if(initialized)CefShutdown();return 1;}
}
