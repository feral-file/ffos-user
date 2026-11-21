#include "ff/display.hpp"
#include "ff/media_ipc.hpp"
#include "ff/presentation_feedback.hpp"
#include <mpv/client.h>
#include <mpv/render_gl.h>
#include <SDL.h>
#include <SDL_syswm.h>
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES3/gl3.h>
#include <atomic>
#include <chrono>
#include <csignal>
#include <iostream>
#include <poll.h>
#include <set>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <unistd.h>

namespace {
constexpr int channel = 3;
std::atomic_bool render_update{true};
void wake(void*) { render_update.store(true); }
void* resolve_gl(void*, const char* name) { return SDL_GL_GetProcAddress(name); }
void check(int result, const char* action) { ff::require(result >= 0, "media_backend", std::string(action)+": "+mpv_error_string(result)); }
void option(mpv_handle* player, const char* name, const char* value) { check(mpv_set_option_string(player,name,value),name); }
void property(mpv_handle* player, const char* name, const std::string& value,uint64_t tag=0) {
  const char* text = value.c_str(); check(mpv_set_property_async(player,tag,name,MPV_FORMAT_STRING,&text),name);
}
void command(mpv_handle* player, const std::vector<std::string>& words) {
  std::vector<const char*> args; for (const auto& word:words)args.push_back(word.c_str()); args.push_back(nullptr);
  check(mpv_command_async(player,0,args.data()),"media command");
}
// No device product state is held here. One worker renders one occurrence.
class Worker {
 public:
  ~Worker() {
    feedback_.reset();
    if(render_)mpv_render_context_free(render_);
    if(player_)mpv_terminate_destroy(player_);
    if(context_)SDL_GL_DeleteContext(context_);
    if(window_)SDL_DestroyWindow(window_);
    SDL_Quit();if(cached_fd_>=0)close(cached_fd_);
  }
  void initialize(const ff::Json& config,int cached_fd=-1) {
    width_=config.at("width").get<int>();height_=config.at("height").get<int>();
    ff::require(width_>0&&height_>0&&width_<=8192&&height_<=8192,"media_protocol","Invalid render dimensions");
    ff::require(SDL_Init(SDL_INIT_VIDEO)==0,"media_graphics",SDL_GetError());
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_PROFILE_MASK,SDL_GL_CONTEXT_PROFILE_ES);
    SDL_GL_SetAttribute(SDL_GL_CONTEXT_MAJOR_VERSION,3);SDL_GL_SetAttribute(SDL_GL_CONTEXT_MINOR_VERSION,0);
    const bool direct=config.value("diagnosticDirectOutput",false);
    const auto flags=SDL_WINDOW_OPENGL | (direct ? SDL_WINDOW_FULLSCREEN_DESKTOP : SDL_WINDOW_HIDDEN);
    window_=SDL_CreateWindow("FF media context",0,0,direct?width_:16,direct?height_:16,flags);
    ff::require(window_!=nullptr,"media_graphics",SDL_GetError());
    context_=SDL_GL_CreateContext(window_);ff::require(context_!=nullptr,"media_graphics",SDL_GetError());
    SDL_SysWMinfo wm{};SDL_VERSION(&wm.version);
    ff::require(SDL_GetWindowWMInfo(window_,&wm)&&wm.subsystem==SDL_SYSWM_WAYLAND,"media_graphics","Wayland EGL display required");
    if(direct){
      SDL_ShowCursor(SDL_DISABLE);
      SDL_GL_GetDrawableSize(window_,&width_,&height_);
      ff::require(SDL_GL_SetSwapInterval(0)==0,"media_graphics",SDL_GetError());
      feedback_=std::make_unique<ff::PresentationFeedback>(window_,true);
    }
    player_=mpv_create();ff::require(player_!=nullptr,"media_backend","Cannot allocate libmpv");
    for(auto [key,value]:{std::pair{"config","no"},{"input-default-bindings","no"},
        {"input-vo-keyboard","no"},{"input-terminal","no"},{"terminal","no"},{"osd-level","0"},
        {"image-display-duration","inf"},{"vo","libmpv"},{"hwdec","auto-safe"},{"idle","yes"},{"keep-open","no"},{"audio-display","no"},{"force-window","no"},
        // Preserve artwork alpha and transparent letterboxes. The compositor
        // paints the stage once behind both layers during a crossfade.
        {"background","none"},{"background-color","#00000000"},
        // FFmpeg's CPU-count default retains many full-size reference frames.
        // Bound fallback decoding before the worker's address-space ceiling,
        // especially for 4096-square H.264 outside this GPU's VA-API limits.
        {"vd-lavc-threads","4"},
        {"demuxer-max-bytes","64MiB"},{"demuxer-max-back-bytes","16MiB"},{"cache-on-disk","no"},
        {"tls-verify","yes"},{"network-timeout","15"},{"load-unsafe-playlists","no"},{"sub-auto","no"},{"audio-file-auto","no"},
        {"demuxer-lavf-o","protocol_whitelist=[http,https,tcp,tls,crypto,data,file,fd]"}})option(player_,key,value);
    if(auto agent=config.value("httpUserAgent","");!agent.empty())option(player_,"user-agent",agent.c_str());
    check(mpv_initialize(player_),"initialize libmpv");
    for(auto name:{"scale","dscale"}){
      char* value=mpv_get_property_string(player_,name);
      ff::require(value,"media_backend","Cannot read the configured video scaler");
      default_scalers_.emplace_back(value);mpv_free(value);
    }
    mpv_request_log_messages(player_,"warn");
    for(auto name:{"time-pos","duration","seekable","hwdec-current","video-codec","frame-drop-count","decoder-frame-drop-count","eof-reached",
                   "video-params/w","video-params/h","video-params/dw","video-params/dh","video-params/rotate","video-params/pixelformat","current-ao","audio-params/samplerate","audio-params/channel-count","pause","volume","mute"})
      check(mpv_observe_property(player_,0,name,MPV_FORMAT_NODE),"observe media");
    mpv_opengl_init_params init{resolve_gl,nullptr};int advanced=1;
    mpv_render_param params[]={{MPV_RENDER_PARAM_API_TYPE,const_cast<char*>(MPV_RENDER_API_TYPE_OPENGL)},
      {MPV_RENDER_PARAM_OPENGL_INIT_PARAMS,&init},{MPV_RENDER_PARAM_WL_DISPLAY,wm.info.wl.display},
      {MPV_RENDER_PARAM_ADVANCED_CONTROL,&advanced},{MPV_RENDER_PARAM_INVALID,nullptr}};
    check(mpv_render_context_create(&render_,player_,params),"create GPU render context");
    mpv_render_context_set_update_callback(render_,wake,nullptr);
    // Enabling this before render-context creation races libmpv's VO startup.
    // Audio still gets a compositable surface in front of the host matte.
    property(player_,"force-window","yes");
    if(!direct){
      glGenTextures(1,&texture_);glBindTexture(GL_TEXTURE_2D,texture_);
      glTexImage2D(GL_TEXTURE_2D,0,GL_RGBA8,width_,height_,0,GL_RGBA,GL_UNSIGNED_BYTE,nullptr);
      glTexParameteri(GL_TEXTURE_2D,GL_TEXTURE_MIN_FILTER,GL_LINEAR);glTexParameteri(GL_TEXTURE_2D,GL_TEXTURE_MAG_FILTER,GL_LINEAR);
      glGenFramebuffers(1,&fbo_);glBindFramebuffer(GL_FRAMEBUFFER,fbo_);
      glFramebufferTexture2D(GL_FRAMEBUFFER,GL_COLOR_ATTACHMENT0,GL_TEXTURE_2D,texture_,0);
      ff::require(glCheckFramebufferStatus(GL_FRAMEBUFFER)==GL_FRAMEBUFFER_COMPLETE,"media_graphics","Media FBO incomplete");
    }
    controls(config);
    auto source=ff::string_field(config,"source",16384);
    ff::require(cached_fd>=0||source.starts_with("https://")||source.starts_with("http://")||source.starts_with("data:"),"media_source","Expected HTTP(S), inline media, or a broker-opened cached file");
    if(cached_fd>=0){cached_fd_=dup(cached_fd);ff::require(cached_fd_>=0,"cache_file","Cannot retain cached media descriptor");source="fd://"+std::to_string(cached_fd_);}
    command(player_,{"loadfile",source,"replace"});
    ff::send_media(channel,{{"event","initialized"},{"version",mpv_client_api_version()}});
  }
  void controls(const ff::Json& p) {
    if(p.contains("directOutput"))want_direct_=p.at("directOutput").get<bool>();
    if(player_ && p.contains("paused"))property(player_,"pause",p.at("paused").get<bool>()?"yes":"no");
    if(player_ && p.contains("volume"))property(player_,"volume",std::to_string(std::clamp(p.at("volume").get<double>(),0.0,1.0)*100));
    if(player_ && p.contains("muted"))property(player_,"mute",p.at("muted").get<bool>()?"yes":"no");
    if(player_ && p.contains("seekSeconds"))command(player_,{"seek",std::to_string(p.at("seekSeconds").get<double>()),"absolute+exact"});
    if(player_ && p.contains("display")&&p.value("displayRevision",uint64_t(1))!=requested_display_){
      requested_display_=p.value("displayRevision",uint64_t(1));
      display_=p.at("display");configure("loop-file",display_.value("loop",true)?"inf":"no");
      auto scaling=display_.value("scaling","fit");
      configure("keepaspect",scaling=="stretch"?"no":"yes");
      configure("panscan",scaling=="fill"?"1":"0");
    }
    if(p.contains("width")&&p.contains("height")){
      pending_width_=p.at("width").get<int>();pending_height_=p.at("height").get<int>();
      ff::require(pending_width_>0&&pending_height_>0&&pending_width_<=8192&&pending_height_<=8192,"media_protocol","Invalid resize dimensions");
    }
  }
  void run() {
    bool in_flight=false,pending=false;
    uint64_t sequence=0,presented=0;
    auto heartbeat=std::chrono::steady_clock::now();
    while(true) {
      if(std::chrono::steady_clock::now()-heartbeat>=std::chrono::seconds(1)){
        ff::send_media(channel,{{"event","heartbeat"}});heartbeat=std::chrono::steady_clock::now();
      }
      pollfd socket{channel,POLLIN,0};poll(&socket,1,2);
      for(int i=0;i<8;++i){
        auto packet=ff::receive_media(channel);if(!packet)break;
        auto method=ff::string_field(packet->body,"method",32);
        if(method=="stop"){
          if(feedback_)std::cout<<feedback_->metrics().dump()<<'\n';
          return;
        }
        if(method=="ack") {ff::require(in_flight&&packet->body.at("sequence")==sequence,"media_protocol","Unexpected frame acknowledgement");in_flight=false;}
        else if(method=="presented") {
          auto frame=packet->body.at("sequence").get<uint64_t>();
          ff::require(frame>presented&&frame<=sequence,"media_protocol","Invalid presentation acknowledgement");
          presented=frame;if(render_)mpv_render_context_report_swap(render_);
        }
        else if(method=="controls")controls(packet->body);
        else throw ff::Error("media_protocol","Unknown host command");
      }
      for(int i=0;player_ && i<64;++i){
        auto event=mpv_wait_event(player_,0);if(event->event_id==MPV_EVENT_NONE)break;
        if(event->event_id==MPV_EVENT_LOG_MESSAGE){
          auto log=static_cast<mpv_event_log_message*>(event->data);std::string text=log->text;
          for(size_t at=text.find("http");at!=std::string::npos;at=text.find("http",at+4)){auto query=text.find('?',at),end=text.find_first_of(" \t\r\n",at);if(query!=std::string::npos&&(end==std::string::npos||query<end))text.replace(query,(end==std::string::npos?text.size():end)-query,"?<redacted>");}
          if(std::string(log->level)=="error")last_error_=text.substr(0,384);
          std::cerr<<"media "<<log->prefix<<": "<<text.substr(0,512);
        }
        if(event->event_id==MPV_EVENT_PLAYBACK_RESTART)ff::send_media(channel,{{"event","loaded"}});
        if(event->event_id==MPV_EVENT_END_FILE){auto end=static_cast<mpv_event_end_file*>(event->data);
          if(end->reason==MPV_END_FILE_REASON_EOF)ff::send_media(channel,{{"event","ended"}});
          if(end->reason==MPV_END_FILE_REASON_ERROR)ff::send_media(channel,{{"event","error"},{"message",std::string(mpv_error_string(end->error))+(last_error_.empty()?"":": "+last_error_)}});
        }
        if(event->event_id==MPV_EVENT_PROPERTY_CHANGE){auto value=static_cast<mpv_event_property*>(event->data);
          if(value->format==MPV_FORMAT_NODE&&value->data){auto node=static_cast<mpv_node*>(value->data);ff::Json v=nullptr;
            if(node->format==MPV_FORMAT_STRING)v=node->u.string;
            if(node->format==MPV_FORMAT_INT64)v=node->u.int64;
            if(node->format==MPV_FORMAT_DOUBLE)v=node->u.double_;
            if(node->format==MPV_FORMAT_FLAG)v=bool(node->u.flag);
            if(v.is_number_integer()){
              std::string_view name=value->name;
              if(name=="video-params/dw")source_width_=v.get<int>();
              if(name=="video-params/dh")source_height_=v.get<int>();
              if(name=="video-params/rotate")source_rotation_=v.get<int>();
            }
            ff::send_media(channel,{{"event","property"},{"name",value->name},{"value",v}});
          }
        }
        if(event->event_id==MPV_EVENT_COMMAND_REPLY||event->event_id==MPV_EVENT_SET_PROPERTY_REPLY)
          if(event->error<0)ff::send_media(channel,{{"event","error"},{"message",mpv_error_string(event->error)}});
        if(event->event_id==MPV_EVENT_SET_PROPERTY_REPLY&&configuration_tags_.erase(event->reply_userdata)&&configuration_tags_.empty()){
          display_revision_=requested_display_;configuration_dirty_=true;
        }
      }
      if(want_direct_ && *want_direct_!=bool(feedback_) && !in_flight){
        if(*want_direct_){
          SDL_SetWindowSize(window_,width_,height_);
          ff::require(SDL_SetWindowFullscreen(window_,SDL_WINDOW_FULLSCREEN_DESKTOP)==0,"media_graphics",SDL_GetError());
          SDL_ShowWindow(window_);SDL_ShowCursor(SDL_DISABLE);
          SDL_GL_SetSwapInterval(0);
          feedback_=std::make_unique<ff::PresentationFeedback>(window_,false);
          glBindFramebuffer(GL_FRAMEBUFFER,0);
        }else{
          feedback_.reset();SDL_HideWindow(window_);
          glBindFramebuffer(GL_FRAMEBUFFER,fbo_);
          ff::send_media(channel,{{"event","property"},{"name","presentationPath"},{"value","composited"}});
        }
        pending=true;direct_reported_=false;
      }
      if(feedback_){
        SDL_Event event;
        bool moved=false, clicked=false;int x=0,y=0;
        while(SDL_PollEvent(&event)){
          if(event.type==SDL_QUIT)return;
          if(event.type==SDL_WINDOWEVENT &&
             (event.window.event==SDL_WINDOWEVENT_EXPOSED ||
              event.window.event==SDL_WINDOWEVENT_SIZE_CHANGED ||
              event.window.event==SDL_WINDOWEVENT_SHOWN))pending=true;
          if(event.type==SDL_MOUSEMOTION && (event.motion.xrel || event.motion.yrel)){
            moved=true;x=event.motion.x;y=event.motion.y;
          }else if(event.type==SDL_MOUSEBUTTONDOWN || event.type==SDL_MOUSEBUTTONUP){
            moved=true;x=event.button.x;y=event.button.y;
            clicked |= event.type==SDL_MOUSEBUTTONDOWN;
          }
        }
        if(moved)ff::send_media(channel,{{"event","pointer"},{"x",x},{"y",y},{"clicked",clicked}});
        feedback_->poll();
        if(!direct_reported_ && feedback_->latest().timestamp_ns){
          ff::send_media(channel,{{"event","property"},{"name","presentationPath"},{"value","cage"}});
          direct_reported_=true;
          // A newly mapped fullscreen surface can receive its initial configure
          // after the first swap. Still and one-frame movies have no next video
          // update to repaint the now-visible drawable.
          pending=true;
        }
        int drawable_width=0,drawable_height=0;
        SDL_GL_GetDrawableSize(window_,&drawable_width,&drawable_height);
        if(drawable_width>0 && drawable_height>0 && (width_!=drawable_width || height_!=drawable_height)){
          width_=drawable_width;height_=drawable_height;pending=true;
        }
      }
      // update() must keep servicing mpv's resource requests even while the
      // exported frame waits for the host. Only the texture write waits.
      if(render_ && render_update.exchange(false))pending |= bool(mpv_render_context_update(render_)&MPV_RENDER_UPDATE_FRAME);
      if(!feedback_&&!in_flight&&pending_width_>0&&(pending_width_!=width_||pending_height_!=height_)){
        // Never change storage while the host still owns an exported frame.
        width_=pending_width_;height_=pending_height_;glBindTexture(GL_TEXTURE_2D,texture_);
        glTexImage2D(GL_TEXTURE_2D,0,GL_RGBA8,width_,height_,0,GL_RGBA,GL_UNSIGNED_BYTE,nullptr);pending=true;
      }
      // Set filtering before advertising a frame at the new display revision.
      // Paused videos also repaint when mpv acknowledges the last scaler change.
      if(player_ && (pending||configuration_dirty_))configure_scaler();
      if(configuration_tags_.empty()&&(pending||configuration_dirty_)&&!in_flight){draw(++sequence);pending=false;configuration_dirty_=false;in_flight=!feedback_;}
    }
  }
 private:
  void configure(const char* name,const std::string& value){
    ff::require(configuration_tags_.size()<4096,"media_protocol","Too many pending media configurations");
    const auto tag=++configuration_tag_;
    configuration_tags_.insert(tag);
    property(player_,name,value,tag);
  }
  void configure_scaler(){
    const bool sideways=source_rotation_%180!=0;
    const bool hard=ff::hard_pixel_edges(display_,sideways?source_height_:source_width_,
                                         sideways?source_width_:source_height_,width_,height_);
    if(hard!=hard_edges_){
      configure("scale",hard?"nearest":default_scalers_[0]);
      configure("dscale",hard?"nearest":default_scalers_[1]);
      hard_edges_=hard;
    }
  }
  void draw(uint64_t sequence) {
    // Host composition samples the top row at v=0, matching CEF DMA-BUFs.
    // MPV's additional Y flip would invert video while leaving native UI upright.
    mpv_opengl_fbo target{static_cast<int>(feedback_?0:fbo_),width_,height_,feedback_?0:GL_RGBA8};int flip=feedback_?1:0;
    mpv_render_param params[]={{MPV_RENDER_PARAM_OPENGL_FBO,&target},{MPV_RENDER_PARAM_FLIP_Y,&flip},{MPV_RENDER_PARAM_INVALID,nullptr}};
    check(mpv_render_context_render(render_,params),"render media frame");
    if(feedback_){
      // In direct mode mpv owns the visible window. There is no exported
      // texture to acknowledge and no native host between the worker and Cage.
      feedback_->before_swap(sequence,sequence);
      SDL_GL_SwapWindow(window_);
      if(render_)mpv_render_context_report_swap(render_);
      return;
    }
    glFinish();
    auto create=reinterpret_cast<PFNEGLCREATEIMAGEKHRPROC>(eglGetProcAddress("eglCreateImageKHR"));
    auto query=reinterpret_cast<PFNEGLEXPORTDMABUFIMAGEQUERYMESAPROC>(eglGetProcAddress("eglExportDMABUFImageQueryMESA"));
    auto export_image=reinterpret_cast<PFNEGLEXPORTDMABUFIMAGEMESAPROC>(eglGetProcAddress("eglExportDMABUFImageMESA"));
    auto destroy=reinterpret_cast<PFNEGLDESTROYIMAGEKHRPROC>(eglGetProcAddress("eglDestroyImageKHR"));
    ff::require(create&&query&&export_image&&destroy,"media_graphics","DMA-BUF export unavailable");
    auto display=eglGetCurrentDisplay();EGLint attrs[]={EGL_GL_TEXTURE_LEVEL_KHR,0,EGL_NONE};
    auto image=create(display,eglGetCurrentContext(),EGL_GL_TEXTURE_2D_KHR,reinterpret_cast<EGLClientBuffer>(static_cast<uintptr_t>(texture_)),attrs);
    ff::require(image!=EGL_NO_IMAGE_KHR,"media_graphics","Cannot export media texture");
    int format=0,count=0;EGLuint64KHR modifier=0;
    bool valid=query(display,image,&format,&count,&modifier)&&count>0&&count<=4;
    int fds[4]={-1,-1,-1,-1};EGLint stride[4]{},offset[4]{};
    if(valid)valid=export_image(display,image,fds,stride,offset);destroy(display,image);
    ff::MediaPacket exported;
    for(int fd:fds)if(fd>=0)exported.descriptors.push_back(fd);
    ff::require(valid,"media_graphics","Cannot export DMA-BUF planes");
    ff::Json planes=ff::Json::array();for(int i=0;i<count;++i)planes.push_back({{"stride",stride[i]},{"offset",offset[i]}});
    ff::send_media(channel,{{"event","frame"},{"sequence",sequence},{"displayRevision",display_revision_},{"width",width_},{"height",height_},{"format",format},{"modifier",modifier},{"planes",planes}},exported.descriptors);
  }
  std::unique_ptr<ff::PresentationFeedback> feedback_;
  SDL_Window* window_=nullptr;SDL_GLContext context_=nullptr;
  mpv_handle* player_=nullptr;mpv_render_context* render_=nullptr;
  ff::Json display_=ff::Json::object();
  std::vector<std::string> default_scalers_;
  int source_width_=0,source_height_=0,source_rotation_=0;
  bool hard_edges_=false;
  std::optional<bool> want_direct_;
  bool direct_reported_=false;
  std::string last_error_;
  int cached_fd_=-1;
  int width_=0,height_=0;GLuint texture_=0,fbo_=0;
  int pending_width_=0,pending_height_=0;
  uint64_t display_revision_=0;
  uint64_t requested_display_=0,configuration_tag_=0;
  std::set<uint64_t> configuration_tags_;
  bool configuration_dirty_=false;
};
}
int main() {
  // Media faults and unbounded allocation are contained in this process.
  // Namespace/filesystem restrictions are applied by the launcher separately.
  prctl(PR_SET_PDEATHSIG,SIGKILL);prctl(PR_SET_NO_NEW_PRIVS,1,0,0,0);
  rlimit limit{3ULL*1024*1024*1024,3ULL*1024*1024*1024};setrlimit(RLIMIT_AS,&limit);
  std::signal(SIGPIPE,SIG_IGN);
  try {
    pollfd socket{channel,POLLIN,0};ff::require(poll(&socket,1,5000)>0,"media_protocol","Host did not configure media worker");
    auto first=ff::receive_media(channel);ff::require(first.has_value(),"media_protocol","Missing configuration");
    Worker worker;worker.initialize(first->body,first->descriptors.empty()?-1:first->descriptors.at(0));worker.run();return 0;
  }catch(const std::exception& error){
    std::cerr<<"media worker: "<<error.what()<<'\n';
    try{ff::send_media(channel,{{"event","error"},{"message",std::string(error.what()).substr(0,512)}});}catch(...){}
    return 1;
  }
}
