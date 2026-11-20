#include "ff/raster.hpp"
#include <glycin.h>
#include <curl/curl.h>
#include <algorithm>
#include <cmath>
#include <future>
#include <utility>
#include <sys/stat.h>
#include <unistd.h>
#include <vector>

namespace ff {
namespace {
constexpr size_t max_bytes = 256ULL << 20;
using Clock = std::chrono::steady_clock;
template<class T> using Object = std::unique_ptr<T, decltype(&g_object_unref)>;
template<class T> Object<T> take(T* obj) { return {obj, g_object_unref}; }
size_t append(char* input, size_t size, size_t count, void* dest) {
  auto& bytes = *static_cast<std::vector<char>*>(dest);
  if (size && count > (max_bytes - bytes.size()) / size) return 0;
  try { bytes.insert(bytes.end(), input, input + count * size); }
  catch (...) { return 0; }
  return count * size;
}
Object<GlyImage> open_image(std::string source, int fd, std::string agent) {
  std::vector<char> bytes;
  if (fd >= 0) {
    struct stat info{};
    require(fstat(fd, &info)==0 && info.st_size>0 && uint64_t(info.st_size)<=max_bytes,
            "image_source","Invalid cached image size");
    bytes.resize(info.st_size);
    for (size_t pos=0; pos<bytes.size();) {
      auto n=pread(fd,bytes.data()+pos,bytes.size()-pos,pos);
      require(n>0,"image_source","Cannot read cached image");
      pos+=n;
    }
  } else {
    std::unique_ptr<CURL,decltype(&curl_easy_cleanup)> curl(curl_easy_init(),curl_easy_cleanup);
    require(bool(curl),"image_source","Cannot initialize HTTP");
    curl_easy_setopt(curl.get(), CURLOPT_URL, source.c_str());
    curl_easy_setopt(curl.get(), CURLOPT_PROTOCOLS_STR,"http,https");
    curl_easy_setopt(curl.get(), CURLOPT_REDIR_PROTOCOLS_STR,"http,https");
    curl_easy_setopt(curl.get(), CURLOPT_FOLLOWLOCATION,1L);
    curl_easy_setopt(curl.get(), CURLOPT_MAXREDIRS,8L);
    curl_easy_setopt(curl.get(), CURLOPT_TIMEOUT,60L);
    curl_easy_setopt(curl.get(), CURLOPT_FAILONERROR,1L);
    if (!agent.empty()) curl_easy_setopt(curl.get(),CURLOPT_USERAGENT,agent.c_str());
    curl_easy_setopt(curl.get(), CURLOPT_WRITEFUNCTION,append);
    curl_easy_setopt(curl.get(), CURLOPT_WRITEDATA,&bytes);
    auto result=curl_easy_perform(curl.get());
    require(result==CURLE_OK,"image_source",curl_easy_strerror(result));
  }
  std::unique_ptr<GBytes,decltype(&g_bytes_unref)> encoded(g_bytes_new(bytes.data(),bytes.size()),g_bytes_unref);
  auto loader=take(gly_loader_new_for_bytes(encoded.get()));
  gly_loader_set_sandbox_selector(loader.get(),GLY_SANDBOX_SELECTOR_BWRAP);
  gly_loader_set_accepted_memory_formats(loader.get(),GLY_MEMORY_SELECTION_R8G8B8A8_PREMULTIPLIED);
  GError* error=nullptr;
  auto image=take(gly_loader_load(loader.get(),&error));
  if (!image) {
    std::string reason=error?error->message:"Cannot load image";
    g_clear_error(&error); throw Error("image_decode",reason);
  }
  require(uint64_t(gly_image_get_width(image.get()))*gly_image_get_height(image.get())<=100000000,
          "image_size","Image exceeds the FF1 decoded pixel budget");
  return image;
}
}
struct Raster::Impl {
  std::future<Object<GlyImage>> loading;
  Object<GlyImage> image=take<GlyImage>(nullptr);
  GLuint texture=0, framebuffer=0;
  int width=0,height=0;
  bool exhausted=false,ended=false;
  Clock::time_point next=Clock::time_point::max();
};
Raster::Raster(const std::string& source,int fd,const std::string& agent):impl_(std::make_unique<Impl>()) {
  impl_->loading=std::async(std::launch::async,open_image,source,fd,agent);
  glGenTextures(1,&impl_->texture); glGenFramebuffers(1,&impl_->framebuffer);
}
Raster::~Raster(){
  glDeleteFramebuffers(1,&impl_->framebuffer); glDeleteTextures(1,&impl_->texture);
}
bool Raster::update(bool paused,bool loop) {
  auto& s=*impl_;
  if(!s.image){
    if(s.loading.wait_for(std::chrono::seconds(0))!=std::future_status::ready)return false;
    s.image=s.loading.get();
  }else if(paused || (Clock::now()<s.next && !(loop && s.exhausted)))return false;
  s.exhausted=false;
  auto request=take(gly_frame_request_new());
  gly_frame_request_set_loop_animation(request.get(),loop);
  GError* err=nullptr;
  auto frame=take(gly_image_get_specific_frame(s.image.get(),request.get(),&err));
  if(!frame){
    if(err && err->domain==GLY_LOADER_ERROR && err->code==GLY_LOADER_ERROR_NO_MORE_FRAMES){
      g_clear_error(&err);s.exhausted=true;s.ended=true;s.next=Clock::time_point::max();return false;
    }
    std::string reason=err?err->message:"Cannot decode image frame";
    g_clear_error(&err);throw Error("image_decode",reason);
  }
  auto delay=gly_frame_get_delay(frame.get());
  s.next=delay>0?Clock::now()+std::chrono::microseconds(delay):Clock::time_point::max();
  s.width=gly_frame_get_width(frame.get());s.height=gly_frame_get_height(frame.get());
  auto stride=gly_frame_get_stride(frame.get());
  auto* pixels=gly_frame_get_buf_bytes(frame.get());
  gsize len=0;auto data=g_bytes_get_data(pixels,&len);
  require(s.width>0 && s.height>0 && stride>=4ULL*s.width && stride%4==0 &&
          uint64_t(stride)*(s.height-1)+4ULL*s.width<=len,"image_decode","Invalid raster layout");
  glBindTexture(GL_TEXTURE_2D,s.texture);
  glPixelStorei(GL_UNPACK_ROW_LENGTH,stride/4);
  glTexImage2D(GL_TEXTURE_2D,0,GL_RGBA8,s.width,s.height,0,GL_RGBA,GL_UNSIGNED_BYTE,data);
  glPixelStorei(GL_UNPACK_ROW_LENGTH,0);
  glBindFramebuffer(GL_FRAMEBUFFER,s.framebuffer);
  glFramebufferTexture2D(GL_FRAMEBUFFER,GL_COLOR_ATTACHMENT0,GL_TEXTURE_2D,s.texture,0);
  require(glCheckFramebufferStatus(GL_FRAMEBUFFER)==GL_FRAMEBUFFER_COMPLETE,"image_graphics","Invalid image framebuffer");
  return true;
}
bool Raster::take_ended(){return std::exchange(impl_->ended,false);}
void Raster::draw(GLuint target,int width,int height,const Json& display,bool direct){
  auto& s=*impl_;
  glBindFramebuffer(GL_DRAW_FRAMEBUFFER,target);
  glDisable(GL_SCISSOR_TEST);
  glClearColor(0,0,0,direct?1:0);glClear(GL_COLOR_BUFFER_BIT);
  auto b=direct?display_bounds(display,width,height):Bounds{0,0,width,height};
  int w=b.width,h=b.height;
  auto mode=display.value("scaling","fit");
  if(mode!="stretch"){
    auto x=double(w)/s.width,y=double(h)/s.height;
    auto scale=mode=="fill"?std::max(x,y):std::min(x,y);
    w=std::max(1,int(std::round(s.width*scale)));h=std::max(1,int(std::round(s.height*scale)));
  }
  int x=b.x+(b.width-w)/2,y=b.y+(b.height-h)/2;
  glEnable(GL_SCISSOR_TEST);glScissor(b.x,b.y,b.width,b.height);
  glBindFramebuffer(GL_READ_FRAMEBUFFER,s.framebuffer);
  glBlitFramebuffer(0,0,s.width,s.height,x,direct?y+h:y,x+w,direct?y:y+h,GL_COLOR_BUFFER_BIT,
                    hard_pixel_edges(display,s.width,s.height,b.width,b.height)?GL_NEAREST:GL_LINEAR);
  glDisable(GL_SCISSOR_TEST);glBindFramebuffer(GL_FRAMEBUFFER,target);
}
}
