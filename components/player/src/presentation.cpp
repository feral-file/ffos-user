#include "ff/presentation.hpp"
#include "ff/overlay.hpp"
#include <EGL/eglext.h>
#include <GLES2/gl2ext.h>
#include <drm_fourcc.h>
#include <pango/pangocairo.h>
#include <qrencode.h>
#include <chrono>
#include <cmath>
#include <iostream>

namespace ff {
void FrameTiming::add(double milliseconds) {
  ++count; total += milliseconds; maximum = std::max(maximum, milliseconds);
  ++bins[std::min<size_t>(bins.size()-1, static_cast<size_t>(milliseconds))];
}
Json FrameTiming::json() const {
  auto percentile = [&](double p) {
    if (!count) return size_t(0);
    uint64_t sum = 0;
    for (size_t i=0; i<bins.size(); ++i) {
      sum += bins[i];
      if (sum >= std::max<uint64_t>(1, static_cast<uint64_t>(std::ceil(count*p)))) return i;
    }
    return bins.size()-1;
  };
  return {{"count",count},{"meanMs",count?total/count:0},{"maxMs",maximum},
          {"p50BinMs",percentile(.5)},{"p95BinMs",percentile(.95)},{"p99BinMs",percentile(.99)}};
}
namespace {
GLuint shader(GLenum type, const char* source) {
  auto handle = glCreateShader(type); glShaderSource(handle, 1, &source, nullptr); glCompileShader(handle);
  GLint ok; glGetShaderiv(handle, GL_COMPILE_STATUS, &ok);
  require(ok, "graphics_error", "Native composition shader failed to compile"); return handle;
}
void allocate(Surface& s, int width, int height) {
  require(width > 0 && height > 0 && width <= 16384 && height <= 16384, "graphics_error", "Invalid surface dimensions");
  if (!s.texture) glGenTextures(1, &s.texture);
  glBindTexture(GL_TEXTURE_2D, s.texture);
  if (s.width != width || s.height != height) {
    s.width = width; s.height = height;
    glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, width, height, 0, GL_RGBA, GL_UNSIGNED_BYTE, nullptr);
    glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_LINEAR); glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_LINEAR);
    glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_S, GL_CLAMP_TO_EDGE); glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_T, GL_CLAMP_TO_EDGE);
  }
}

}
Presentation::Presentation(bool diagnostic_cpu, bool timing_samples, const std::string& fonts) : diagnostic_cpu_(diagnostic_cpu) {
  load_device_fonts(fonts);
  require(SDL_Init(SDL_INIT_VIDEO | SDL_INIT_EVENTS) == 0, "display_unavailable", SDL_GetError());
  SDL_GL_SetAttribute(SDL_GL_CONTEXT_PROFILE_MASK, SDL_GL_CONTEXT_PROFILE_ES);
  SDL_GL_SetAttribute(SDL_GL_CONTEXT_MAJOR_VERSION, 3); SDL_GL_SetAttribute(SDL_GL_CONTEXT_MINOR_VERSION, 0);
  SDL_GL_SetAttribute(SDL_GL_DOUBLEBUFFER, 1);
  window_ = SDL_CreateWindow("Feral File", SDL_WINDOWPOS_UNDEFINED, SDL_WINDOWPOS_UNDEFINED, 1280, 720, SDL_WINDOW_OPENGL | SDL_WINDOW_FULLSCREEN_DESKTOP | SDL_WINDOW_ALLOW_HIGHDPI);
  require(window_ != nullptr, "display_unavailable", SDL_GetError());
  context_ = SDL_GL_CreateContext(window_); require(context_ != nullptr, "display_unavailable", SDL_GetError());
  // Cage owns display synchronization. Waiting for its next frame callback on
  // CEF's UI thread serializes browser capture behind the previous host swap.
  // Submit without that wait; host composition remains bounded to 60 Hz.
  require(SDL_GL_SetSwapInterval(0) == 0, "graphics_error", SDL_GetError()); SDL_GL_GetDrawableSize(window_, &width_, &height_); SDL_ShowCursor(SDL_DISABLE);
  feedback_ = std::make_unique<PresentationFeedback>(window_, timing_samples);
  SDL_StartTextInput();
  const char* vertex = "#version 300 es\nconst vec2 p[4]=vec2[4](vec2(-1,-1),vec2(1,-1),vec2(-1,1),vec2(1,1));out vec2 uv;void main(){gl_Position=vec4(p[gl_VertexID],0,1);uv=vec2((p[gl_VertexID].x+1.)/2.,(1.-p[gl_VertexID].y)/2.);}";
  const char* fragment = "#version 300 es\nprecision mediump float;in vec2 uv;uniform sampler2D tex;uniform float opacity;out vec4 color;void main(){color=texture(tex,uv)*opacity;}";
  auto vs = shader(GL_VERTEX_SHADER, vertex), fs = shader(GL_FRAGMENT_SHADER, fragment);
  program_ = glCreateProgram(); glAttachShader(program_, vs); glAttachShader(program_, fs); glLinkProgram(program_);
  glDeleteShader(vs); glDeleteShader(fs); GLint ok; glGetProgramiv(program_, GL_LINK_STATUS, &ok); require(ok, "graphics_error", "Composition shader link failed");
  std::cerr << "presentation driver=" << SDL_GetCurrentVideoDriver() << " renderer=" << glGetString(GL_RENDERER) << " size=" << width_ << 'x' << height_ << '\n';
}
Presentation::~Presentation() {
  feedback_.reset();
  glDeleteTextures(1, &ui_); glDeleteProgram(program_);
  if (context_) SDL_GL_DeleteContext(context_); if (window_) SDL_DestroyWindow(window_); SDL_Quit();
}
void Presentation::destroy(Surface& surface) { glDeleteTextures(1, &surface.texture); surface = {}; }
void Presentation::accelerated(Surface& surface, const CefAcceleratedPaintInfo& info) {
  require(SDL_GL_MakeCurrent(window_, context_) == 0, "graphics_error", SDL_GetError());
  const auto started = std::chrono::steady_clock::now();
  require(info.plane_count > 0 && info.plane_count <= 4, "graphics_error", "Invalid DMA-BUF plane count");
  auto create = reinterpret_cast<PFNEGLCREATEIMAGEKHRPROC>(eglGetProcAddress("eglCreateImageKHR"));
  auto release = reinterpret_cast<PFNEGLDESTROYIMAGEKHRPROC>(eglGetProcAddress("eglDestroyImageKHR"));
  auto bind = reinterpret_cast<PFNGLEGLIMAGETARGETTEXTURE2DOESPROC>(eglGetProcAddress("glEGLImageTargetTexture2DOES"));
  require(create && release && bind, "graphics_error", "DMA-BUF EGL import is unavailable");
  int width = info.extra.coded_size.width, height = info.extra.coded_size.height;
  auto format = info.format == CEF_COLOR_TYPE_BGRA_8888 ? DRM_FORMAT_ARGB8888 : DRM_FORMAT_ABGR8888;
  std::vector<EGLint> attrs = {EGL_WIDTH, width, EGL_HEIGHT, height, EGL_LINUX_DRM_FOURCC_EXT, static_cast<EGLint>(format)};
  const EGLint fds[] = {EGL_DMA_BUF_PLANE0_FD_EXT, EGL_DMA_BUF_PLANE1_FD_EXT, EGL_DMA_BUF_PLANE2_FD_EXT, EGL_DMA_BUF_PLANE3_FD_EXT};
  const EGLint offsets[] = {EGL_DMA_BUF_PLANE0_OFFSET_EXT, EGL_DMA_BUF_PLANE1_OFFSET_EXT, EGL_DMA_BUF_PLANE2_OFFSET_EXT, EGL_DMA_BUF_PLANE3_OFFSET_EXT};
  const EGLint pitches[] = {EGL_DMA_BUF_PLANE0_PITCH_EXT, EGL_DMA_BUF_PLANE1_PITCH_EXT, EGL_DMA_BUF_PLANE2_PITCH_EXT, EGL_DMA_BUF_PLANE3_PITCH_EXT};
  const EGLint mods[] = {EGL_DMA_BUF_PLANE0_MODIFIER_LO_EXT, EGL_DMA_BUF_PLANE1_MODIFIER_LO_EXT, EGL_DMA_BUF_PLANE2_MODIFIER_LO_EXT, EGL_DMA_BUF_PLANE3_MODIFIER_LO_EXT};
  for (int i = 0; i < info.plane_count; ++i) {
    // EGL/driver implicit synchronization orders this read after the producer.
    // Do not poll-and-drop: continuously busy artwork can lose every frame.
    // The GPU copy below completes before CEF reuses its borrowed buffer.
    require(info.planes[i].offset <= INT32_MAX, "graphics_error", "DMA-BUF offset exceeds EGL range");
    attrs.insert(attrs.end(), {fds[i], info.planes[i].fd, offsets[i], static_cast<EGLint>(info.planes[i].offset), pitches[i], static_cast<EGLint>(info.planes[i].stride)});
    if (info.modifier != DRM_FORMAT_MOD_INVALID) attrs.insert(attrs.end(), {mods[i], static_cast<EGLint>(info.modifier & 0xffffffff), mods[i] + 1, static_cast<EGLint>(info.modifier >> 32)});
  }
  attrs.push_back(EGL_NONE);
  auto display = eglGetCurrentDisplay(); auto image = create(display, EGL_NO_CONTEXT, EGL_LINUX_DMA_BUF_EXT, nullptr, attrs.data());
  require(image != EGL_NO_IMAGE_KHR, "graphics_error", "DMA-BUF format/modifier cannot be imported");
  GLuint imported, framebuffers[2]; glGenTextures(1, &imported); glBindTexture(GL_TEXTURE_2D, imported); bind(GL_TEXTURE_2D, image);
  glGenFramebuffers(2, framebuffers); glBindFramebuffer(GL_READ_FRAMEBUFFER, framebuffers[0]);
  glFramebufferTexture2D(GL_READ_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, imported, 0);
  const bool source_ok = glCheckFramebufferStatus(GL_READ_FRAMEBUFFER) == GL_FRAMEBUFFER_COMPLETE;
  auto rect = info.extra.visible_rect; if (rect.width <= 0 || rect.height <= 0) rect = {0, 0, width, height};
  allocate(surface, rect.width, rect.height); glBindFramebuffer(GL_DRAW_FRAMEBUFFER, framebuffers[1]);
  glFramebufferTexture2D(GL_DRAW_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, surface.texture, 0);
  if (source_ok) glBlitFramebuffer(rect.x, rect.y, rect.x + rect.width, rect.y + rect.height, 0, 0, rect.width, rect.height, GL_COLOR_BUFFER_BIT, GL_NEAREST);
  // CEF reuses its buffer immediately after this callback. Complete the GPU
  // copy here; never retain CEF's FD/image, and never read pixels on this path.
  // This relies on Mesa's implicit DMA-BUF fences. It does not repair CEF #4313
  // on NVIDIA. A driver/GPU stall is a shared process failure boundary requiring
  // external watchdog recovery, not a recoverable artwork-renderer failure.
  glFinish(); auto error = glGetError(); glBindFramebuffer(GL_FRAMEBUFFER, 0);
  glDeleteFramebuffers(2, framebuffers); glDeleteTextures(1, &imported); release(display, image);
  require(source_ok && error == GL_NO_ERROR, "graphics_error", "DMA-BUF GPU copy failed");
  surface.accelerated = true; ++surface.frames; ++gpu_copies_;
  auto elapsed = std::chrono::duration<double, std::milli>(std::chrono::steady_clock::now() - started).count();
  copy_ms_ += elapsed; copy_timing_.add(elapsed);
}
void Presentation::software(Surface& surface, const void* data, int width, int height) {
  require(diagnostic_cpu_, "acceleration_unavailable", "CEF returned CPU frames; accelerated playback is required");
  require(SDL_GL_MakeCurrent(window_, context_) == 0, "graphics_error", SDL_GetError());
  allocate(surface, width, height);
  // CEF BGRA -> RGBA. This explicit diagnostic path is counted and never
  // silently enabled on a production device.
  std::vector<unsigned char> pixels(static_cast<size_t>(width) * height * 4);
  auto in = static_cast<const unsigned char*>(data);
  for (size_t i = 0; i < pixels.size(); i += 4) { pixels[i] = in[i+2]; pixels[i+1] = in[i+1]; pixels[i+2] = in[i]; pixels[i+3] = in[i+3]; }
  glTexSubImage2D(GL_TEXTURE_2D, 0, 0, 0, width, height, GL_RGBA, GL_UNSIGNED_BYTE, pixels.data());
  ++surface.frames; ++cpu_copies_;
}
Bounds Presentation::bounds(const Json& display) const {
  int margin = 0;
  if (display.contains("margin")) {
    const auto& m = display["margin"];
    if (m.is_number()) margin = static_cast<int>(m.get<double>());
    else if (m.is_string() && m.get<std::string>().ends_with('%')) {
      try { margin = static_cast<int>(std::stod(m.get<std::string>()) * std::min(width_, height_) / 100.0); } catch (...) {}
    }
  }
  margin = std::clamp(margin, 0, std::min(width_, height_) / 3);
  return {margin, margin, width_ - 2*margin, height_ - 2*margin};
}
void Presentation::quad(GLuint texture, Bounds rect, float opacity) {
  glViewport(rect.x, height_ - rect.y - rect.height, rect.width, rect.height); glUseProgram(program_);
  glActiveTexture(GL_TEXTURE0); glBindTexture(GL_TEXTURE_2D, texture); glUniform1i(glGetUniformLocation(program_, "tex"), 0);
  glUniform1f(glGetUniformLocation(program_, "opacity"), opacity); glDrawArrays(GL_TRIANGLE_STRIP, 0, 4);
}
void Presentation::upload_ui(UIRaster raster, const std::string& key) {
  ui_key_=key; ui_raster_=std::move(raster);ui_width_=ui_raster_.width;ui_height_=ui_raster_.height;
  if(!ui_width_||!ui_height_)return;
  if(!ui_)glGenTextures(1,&ui_);glBindTexture(GL_TEXTURE_2D,ui_);
  glTexImage2D(GL_TEXTURE_2D,0,GL_RGBA,ui_width_,ui_height_,0,GL_RGBA,GL_UNSIGNED_BYTE,ui_raster_.rgba.data());
  glTexParameteri(GL_TEXTURE_2D,GL_TEXTURE_MIN_FILTER,GL_LINEAR);glTexParameteri(GL_TEXTURE_2D,GL_TEXTURE_MAG_FILTER,GL_LINEAR);
  // Retain geometry only; the texture owns the cached label/panel pixels.
  ui_raster_.rgba.clear();ui_raster_.rgba.shrink_to_fit();
}
void Presentation::ui(const std::string& text,const std::string&,bool full) {
  auto key=std::string(full?"message:":"notice:")+text+std::to_string(width_)+":"+std::to_string(height_);
  if(key!=ui_key_)upload_ui(render_message(text,width_,height_,full),key);
  quad(ui_,{0,full?0:height_-ui_height_,ui_width_,ui_height_});
}
void Presentation::swap(const Surface* artwork) {
  const auto start = std::chrono::steady_clock::now();
  const bool changed = artwork && (artwork->texture != last_frame_texture_ || artwork->frames != last_frame_number_ || label_occurrence_ != last_frame_occurrence_);
  if (changed) ++frame_serial_;
  feedback_->before_swap(artwork ? frame_serial_ : 0);
  SDL_GL_SwapWindow(window_); ++swaps_;
  const auto end = std::chrono::steady_clock::now();
  swap_timing_.add(std::chrono::duration<double,std::milli>(end-start).count());
  if (changed) {
    if (last_frame_ != std::chrono::steady_clock::time_point{})
      frame_timing_.add(std::chrono::duration<double,std::milli>(end-last_frame_).count());
    last_frame_ = end; last_frame_texture_ = artwork->texture; last_frame_number_ = artwork->frames; last_frame_occurrence_ = label_occurrence_;
  }
}
bool Presentation::draw(const Surface* artwork, const Occurrence* occurrence, const Playback& playback, const Surface* backdrop, float backdrop_opacity) {
  const auto now = std::chrono::steady_clock::now();
  const auto ticks = SDL_GetTicks64();
  const bool pointer_visible = pointer_active_ && ticks-pointer_active_<5000;
  const auto mode = playback.state()["settings"].value("tombstone","timed");
  const bool has_title=occurrence&&has_tombstone(*occurrence);
  if(occurrence&&playback.overlay().is_null()&&backdrop_opacity==0&&
     (label_occurrence_!=occurrence->id||label_mode_!=mode||label_has_title_!=has_title)){
    label_occurrence_=occurrence->id;label_mode_=mode;label_has_title_=has_title;label_started_=ticks;
  }
  const auto label_age=ticks-label_started_;
  const bool label_visible = has_title && (mode=="on" || (mode=="timed" && label_age<30300));
  const int label_phase=label_visible&&mode=="timed"?int(std::min<uint64_t>(label_age,30300)/16):0;
  const DrawKey key{playback.revision(), artwork?artwork->texture:0, artwork?artwork->frames:0,
    occurrence?occurrence->id:"", width_,height_,pointer_visible,label_visible,
    pointer_visible?pointer_x_:0,pointer_visible?pointer_y_:0,label_phase,backdrop?backdrop->texture:0,backdrop?backdrop->frames:0,backdrop_opacity};
  // Resubmitting an unchanged texture still consumes GPU/compositor work and
  // can block the next capture. UI revisions, geometry, pointer motion/expiry
  // and timed label expiry all independently invalidate composition.
  if (draw_key_ == key || !draw_clock_.due(now)) return false;
  draw_key_ = key;
  require(SDL_GL_MakeCurrent(window_, context_) == 0, "graphics_error", SDL_GetError());
  glBindFramebuffer(GL_FRAMEBUFFER,0);glViewport(0,0,width_,height_);glClearColor(0,0,0,1);glClear(GL_COLOR_BUFFER_BIT);
  // Device setup can explicitly own the screen while playback sleeps. In its
  // absence sleep is black, including recent cursor and retained error UI.
  if(playback.state()["sleeping"]==true&&playback.overlay().is_null()){
    swap(nullptr);return true;
  }
  glEnable(GL_BLEND); glBlendFunc(GL_ONE, GL_ONE_MINUS_SRC_ALPHA);
  if (artwork && artwork->texture && occurrence) quad(artwork->texture,bounds(occurrence->display));
  if(backdrop&&backdrop->texture&&backdrop_opacity>0)quad(backdrop->texture,{0,0,width_,height_},backdrop_opacity);
  auto overlay = playback.overlay();
  if (!overlay.is_null()) {
    auto key="panel:"+overlay.dump()+std::to_string(width_)+":"+std::to_string(height_);
    if(key!=ui_key_)upload_ui(render_panel(overlay,width_,height_),key);
    quad(ui_,{0,0,width_,height_});
  } else if (playback.desired() && playback.desired()->stage == "failed") ui("Artwork could not be displayed\n"+playback.desired()->error,"",true);
  else if (playback.desired() && playback.desired()->stage != "presented" && !playback.recovery_error().empty())
    ui("Retrying artwork\n"+playback.recovery_error(),"",!artwork);
  else if (!playback.presentation_failure().is_null())
    ui("Previous artwork stopped\n"+playback.presentation_failure().value("error",""),"",!artwork);
  else if (!playback.storage_error().empty()) ui("Playback state could not be saved\n"+playback.storage_error(),"",false);
  else if (auto outcome=playback.state().value("scheduleOutcome",Json(nullptr));outcome.is_object()&&outcome.value("outcome","")=="failed")
    ui("Scheduled playlist could not start\n"+outcome["error"].value("message","Unknown scheduling failure"),"",false);
  else if (auto outcome=playback.state().value("updateOutcome",Json(nullptr));outcome.is_object()&&outcome.value("contentBlocked",false)&&(!playback.desired()||outcome.value("outcome","")=="failed"))
    ui("Updated playlist is blocked\nChange content preferences or choose another playlist","",!artwork);
  else if (!artwork && playback.state()["sleeping"] == false) ui(playback.desired() ? "Loading artwork" : "Feral File\nReady for artwork","",true);
  else if (occurrence && label_visible) {
    auto key="label:"+occurrence->id+occurrence->item.dump()+occurrence->playlist_title+occurrence->curator+mode+std::to_string(width_)+":"+std::to_string(height_);
    if(key!=ui_key_)upload_ui(render_tombstone(*occurrence,width_,height_,mode=="timed"),key);
    if(ui_width_&&ui_height_){
      const float opacity=mode=="timed"&&label_age>30000?float(30300-label_age)/300:1;
      quad(ui_,{0,height_-ui_height_,ui_width_,ui_height_},opacity);
      if(ui_raster_.timer_width&&opacity==1){
        glEnable(GL_SCISSOR_TEST);
        glScissor(ui_raster_.timer_x,ui_height_-ui_raster_.timer_y-ui_raster_.timer_height,ui_raster_.timer_width,ui_raster_.timer_height);
        glClearColor(227/255.f,227/255.f,227/255.f,1);glClear(GL_COLOR_BUFFER_BIT);
        auto remaining=int(ui_raster_.timer_width*std::max(0.0,1.0-label_age/30000.0));
        glScissor(ui_raster_.timer_x,ui_height_-ui_raster_.timer_y-ui_raster_.timer_height,remaining,ui_raster_.timer_height);
        glClearColor(160/255.f,160/255.f,160/255.f,1);glClear(GL_COLOR_BUFFER_BIT);glDisable(GL_SCISSOR_TEST);
      }
    }
  }
  if (pointer_active_ && SDL_GetTicks64()-pointer_active_<5000 && overlay.is_null()) {
    glEnable(GL_SCISSOR_TEST);glScissor(static_cast<int>(pointer_x_)-5,height_-static_cast<int>(pointer_y_)-5,10,10);glClearColor(1,1,1,1);glClear(GL_COLOR_BUFFER_BIT);glDisable(GL_SCISSOR_TEST);
  }
  swap(artwork);return true;
}
bool Presentation::events(const std::function<void(const Json&)>& input) {
  SDL_Event event;
  while(SDL_PollEvent(&event)) {
    if(event.type==SDL_QUIT)return false;
    // Re-map/exposure can require resubmission even when the artwork and
    // dimensions are unchanged (for example after display reconnection).
    if(event.type==SDL_WINDOWEVENT && (event.window.event==SDL_WINDOWEVENT_EXPOSED ||
       event.window.event==SDL_WINDOWEVENT_SHOWN || event.window.event==SDL_WINDOWEVENT_RESTORED ||
       event.window.event==SDL_WINDOWEVENT_SIZE_CHANGED || event.window.event==SDL_WINDOWEVENT_DISPLAY_CHANGED))
      draw_key_.reset();
    if(event.type==SDL_WINDOWEVENT && event.window.event==SDL_WINDOWEVENT_SIZE_CHANGED)SDL_GL_GetDrawableSize(window_,&width_,&height_);
    if(event.type==SDL_WINDOWEVENT && event.window.event==SDL_WINDOWEVENT_FOCUS_LOST)input({{"kind","releaseAll"}});
    if(event.type==SDL_MOUSEMOTION){
      int logical_width=1,logical_height=1;SDL_GetWindowSize(window_,&logical_width,&logical_height);
      input({{"kind","move"},{"x",double(event.motion.x)*width_/std::max(1,logical_width)},{"y",double(event.motion.y)*height_/std::max(1,logical_height)}});
    }
    if(event.type==SDL_MOUSEBUTTONDOWN||event.type==SDL_MOUSEBUTTONUP)input({{"kind","button"},{"button",event.button.button},{"down",event.type==SDL_MOUSEBUTTONDOWN},{"clicks",event.button.clicks}});
    if(event.type==SDL_MOUSEWHEEL)input({{"kind","wheel"},{"dx",event.wheel.x*40},{"dy",event.wheel.y*40}});
    if(event.type==SDL_TEXTINPUT)input({{"kind","text"},{"text",event.text.text}});
    if(event.type==SDL_KEYDOWN||event.type==SDL_KEYUP)input({{"kind","key"},{"usage",int(event.key.keysym.scancode)},{"down",event.type==SDL_KEYDOWN}});
  }
  feedback_->poll();
  return true;
}
Json Presentation::metrics() const { return {{"gpuCopies",gpu_copies_},{"cpuCopies",cpu_copies_},{"coalescedFrames",coalesced_frames_},{"copyMilliseconds",copy_ms_},{"swaps",swaps_},{"copyTiming",copy_timing_.json()},{"swapTiming",swap_timing_.json()},{"newFrameSubmissionTiming",frame_timing_.json()},{"waylandPresentation",feedback_->metrics()}}; }
}
