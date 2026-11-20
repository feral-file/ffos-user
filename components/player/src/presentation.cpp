#include "ff/presentation.hpp"
#include "ff/overlay.hpp"
#include <EGL/eglext.h>
#include <GLES2/gl2ext.h>
#include <drm_fourcc.h>
#include <pango/pangocairo.h>
#include <qrencode.h>
#include <algorithm>
#include <chrono>
#include <cmath>
#include <iostream>

namespace ff {
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
  SDL_GL_SetAttribute(SDL_GL_CONTEXT_RESET_NOTIFICATION, SDL_GL_CONTEXT_RESET_LOSE_CONTEXT);
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
  browser_overlay_.reset();
  feedback_.reset();
  activate();
  glDeleteTextures(1, &ui_);
  glDeleteTextures(1, &pointer_texture_);
  glDeleteProgram(program_);
  if (context_) SDL_GL_DeleteContext(context_); if (window_) SDL_DestroyWindow(window_); SDL_Quit();
}
bool Presentation::import_read_complete(const Surface& surface) {
  if (!surface.read_fence) return true;
  activate();
  auto status = glClientWaitSync(static_cast<GLsync>(surface.read_fence), 0, 0);
  require(status != GL_WAIT_FAILED, "graphics_error", "Imported texture read fence failed");
  return status != GL_TIMEOUT_EXPIRED;
}
void Presentation::destroy(Surface& surface) {
  activate();
  // Normal reuse has already polled the fence. During occurrence teardown
  // finish the last read before returning the buffer to its Wayland producer.
  if (surface.read_fence) {
    if (!import_read_complete(surface)) glFinish();
    glDeleteSync(static_cast<GLsync>(surface.read_fence));
  }
  if (surface.copy_fence) {
    // Teardown can precede the next poll of a non-RGB browser blit. Keep
    // the scene buffer locked until the copy completes.
    glFinish();
    glDeleteSync(static_cast<GLsync>(surface.copy_fence));
  }
  glDeleteTextures(1, &surface.texture);
  surface = {};
}
void Presentation::activate() {
  // wlroots uses EGL directly, outside SDL's cached current-context state.
  // Invalidate that cache before restoring the native UI context.
  if (eglGetCurrentContext()!=reinterpret_cast<EGLContext>(context_))
    require(SDL_GL_MakeCurrent(window_,nullptr)==0,"graphics_error",SDL_GetError());
  require(SDL_GL_MakeCurrent(window_,context_)==0,"graphics_error",SDL_GetError());
}
void Presentation::accelerated(Surface& surface, const DmaFrame& info, std::shared_ptr<void> producer_buffer, bool direct_import) {
  activate();
  const auto started = std::chrono::steady_clock::now();
  require(info.count > 0 && info.count <= 4, "graphics_error", "Invalid DMA-BUF plane count");
  auto create = reinterpret_cast<PFNEGLCREATEIMAGEKHRPROC>(eglGetProcAddress("eglCreateImageKHR"));
  auto release = reinterpret_cast<PFNEGLDESTROYIMAGEKHRPROC>(eglGetProcAddress("eglDestroyImageKHR"));
  auto bind = reinterpret_cast<PFNGLEGLIMAGETARGETTEXTURE2DOESPROC>(eglGetProcAddress("glEGLImageTargetTexture2DOES"));
  require(create && release && bind, "graphics_error", "DMA-BUF EGL import is unavailable");
  int width = info.width, height = info.height;
  auto format = info.format;
  std::vector<EGLint> attrs = {EGL_WIDTH, width, EGL_HEIGHT, height, EGL_LINUX_DRM_FOURCC_EXT, static_cast<EGLint>(format)};
  const EGLint fds[] = {EGL_DMA_BUF_PLANE0_FD_EXT, EGL_DMA_BUF_PLANE1_FD_EXT, EGL_DMA_BUF_PLANE2_FD_EXT, EGL_DMA_BUF_PLANE3_FD_EXT};
  const EGLint offsets[] = {EGL_DMA_BUF_PLANE0_OFFSET_EXT, EGL_DMA_BUF_PLANE1_OFFSET_EXT, EGL_DMA_BUF_PLANE2_OFFSET_EXT, EGL_DMA_BUF_PLANE3_OFFSET_EXT};
  const EGLint pitches[] = {EGL_DMA_BUF_PLANE0_PITCH_EXT, EGL_DMA_BUF_PLANE1_PITCH_EXT, EGL_DMA_BUF_PLANE2_PITCH_EXT, EGL_DMA_BUF_PLANE3_PITCH_EXT};
  const EGLint mods[] = {EGL_DMA_BUF_PLANE0_MODIFIER_LO_EXT, EGL_DMA_BUF_PLANE1_MODIFIER_LO_EXT, EGL_DMA_BUF_PLANE2_MODIFIER_LO_EXT, EGL_DMA_BUF_PLANE3_MODIFIER_LO_EXT};
  for (int i = 0; i < info.count; ++i) {
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
  if (surface.producer_buffer) destroy(surface);
  GLuint imported, framebuffers[2]; glGenTextures(1, &imported); glBindTexture(GL_TEXTURE_2D, imported); bind(GL_TEXTURE_2D, image);
  // The private Wayland compositor gives us explicit ownership of RGB buffers.
  // Sample their full texture directly. CEF OSR and libmpv callback buffers
  // still require a copy before returning control to the producer.
  const bool rgb = format == DRM_FORMAT_XRGB8888 || format == DRM_FORMAT_ARGB8888 ||
                   format == DRM_FORMAT_XBGR8888 || format == DRM_FORMAT_ABGR8888;
  if (direct_import && producer_buffer && rgb && info.count == 1 && info.visible.width == 0) {
    glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_LINEAR);
    glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_LINEAR);
    glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_S, GL_CLAMP_TO_EDGE);
    glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_T, GL_CLAMP_TO_EDGE);
    release(display, image);
    if (glGetError() != GL_NO_ERROR) {
      glDeleteTextures(1, &imported);
      throw Error("graphics_error", "Cannot sample the imported browser texture");
    }
    glDeleteTextures(1, &surface.texture);
    surface.texture = imported;
    surface.width = width;
    surface.height = height;
    surface.accelerated = true;
    surface.producer_buffer = std::move(producer_buffer);
    ++surface.frames;
    ++direct_imports_;
    return;
  }
  glGenFramebuffers(2, framebuffers); glBindFramebuffer(GL_READ_FRAMEBUFFER, framebuffers[0]);
  glFramebufferTexture2D(GL_READ_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, imported, 0);
  const bool source_ok = glCheckFramebufferStatus(GL_READ_FRAMEBUFFER) == GL_FRAMEBUFFER_COMPLETE;
  Bounds rect{info.visible.x,info.visible.y,info.visible.width,info.visible.height};if(rect.width<=0||rect.height<=0)rect={0,0,width,height};
  allocate(surface, rect.width, rect.height); glBindFramebuffer(GL_DRAW_FRAMEBUFFER, framebuffers[1]);
  glFramebufferTexture2D(GL_DRAW_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, surface.texture, 0);
  if (source_ok) glBlitFramebuffer(rect.x, rect.y, rect.x + rect.width, rect.y + rect.height, 0, 0, rect.width, rect.height, GL_COLOR_BUFFER_BIT, GL_NEAREST);
  // CEF reuses its buffer immediately after this callback. Complete the GPU
  // copy here; never retain CEF's FD/image, and never read pixels on this path.
  // This relies on Mesa's implicit DMA-BUF fences. It does not repair CEF #4313
  // on NVIDIA. A driver/GPU stall is a shared process failure boundary requiring
  // external watchdog recovery, not a recoverable artwork-renderer failure.
  if (producer_buffer) {
    require(!surface.copy_fence,"graphics_error","Copy already in flight");
    surface.copy_started_ns = std::chrono::duration_cast<std::chrono::nanoseconds>(started.time_since_epoch()).count();
    surface.copy_fence = glFenceSync(GL_SYNC_GPU_COMMANDS_COMPLETE,0);
    require(surface.copy_fence,"graphics_error","Cannot fence retained DMA-BUF copy");
    glFlush();
  } else glFinish();
  auto error = glGetError(); glBindFramebuffer(GL_FRAMEBUFFER, 0);
  glDeleteFramebuffers(2, framebuffers); glDeleteTextures(1, &imported); release(display, image);
  require(source_ok && error == GL_NO_ERROR, "graphics_error", "DMA-BUF GPU copy failed");
  surface.accelerated = true; ++surface.frames; ++gpu_copies_;
  auto elapsed = std::chrono::duration<double, std::milli>(std::chrono::steady_clock::now() - started).count();
  copy_ms_ += elapsed; copy_timing_.add(elapsed);
}
bool Presentation::copy_ready(Surface& surface) {
  if(!surface.copy_fence) return true;
  activate();
  auto status = glClientWaitSync(static_cast<GLsync>(surface.copy_fence),0,0);
  require(status != GL_WAIT_FAILED,"graphics_error","DMA-BUF copy fence failed");
  if(status == GL_TIMEOUT_EXPIRED) return false;
  auto now = std::chrono::steady_clock::now().time_since_epoch();
  copy_fence_timing_.add((std::chrono::duration_cast<std::chrono::nanoseconds>(now).count()-surface.copy_started_ns)/1e6);
  glDeleteSync(static_cast<GLsync>(surface.copy_fence));surface.copy_fence=nullptr;
  return true;
}
void Presentation::software(Surface& surface, const void* data, int width, int height) {
  require(diagnostic_cpu_, "acceleration_unavailable", "CEF returned CPU frames; accelerated playback is required");
  activate();
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
  return display_bounds(display,width_,height_);
}
void Presentation::quad(GLuint texture, Bounds rect, float opacity) {
  glViewport(rect.x, height_ - rect.y - rect.height, rect.width, rect.height); glUseProgram(program_);
  glActiveTexture(GL_TEXTURE0); glBindTexture(GL_TEXTURE_2D, texture); glUniform1i(glGetUniformLocation(program_, "tex"), 0);
  glUniform1f(glGetUniformLocation(program_, "opacity"), opacity); glDrawArrays(GL_TRIANGLE_STRIP, 0, 4);
}
void Presentation::artwork_quad(const Surface& surface, Bounds rect, float opacity) {
  quad(surface.texture, rect, opacity);
  if (!surface.producer_buffer) return;
  // UI redraws and crossfades can sample a shown surface more than once.
  // Keep the fence for the most recent read in this GL command stream.
  if (surface.read_fence) glDeleteSync(static_cast<GLsync>(surface.read_fence));
  surface.read_fence = glFenceSync(GL_SYNC_GPU_COMMANDS_COMPLETE, 0);
  if (!surface.read_fence) {
    glFinish();
    throw Error("graphics_error", "Cannot fence an imported browser texture");
  }
}
static void upload_raster(GLuint& texture, const UIRaster& raster) {
  if (!texture) glGenTextures(1, &texture);
  glBindTexture(GL_TEXTURE_2D, texture);
  glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, raster.width, raster.height, 0, GL_RGBA,
               GL_UNSIGNED_BYTE, raster.rgba.data());
  glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_LINEAR);
  glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_LINEAR);
}
void Presentation::draw_pointer() {
  const int size = std::clamp(std::min(width_, height_) / 48, 32, 64);
  if (size != pointer_size_) {
    const auto pointer = render_pointer(size);
    upload_raster(pointer_texture_, pointer.image);
    pointer_size_ = size;
    pointer_hotspot_ = pointer.hotspot;
  }
  quad(pointer_texture_, {static_cast<int>(pointer_x_) - pointer_hotspot_,
                          static_cast<int>(pointer_y_) - pointer_hotspot_, size, size});
}
void Presentation::upload_ui(UIRaster raster, const std::string& key) {
  ui_key_=key; ui_raster_=std::move(raster);ui_width_=ui_raster_.width;ui_height_=ui_raster_.height;
  if(!ui_width_||!ui_height_)return;
  upload_raster(ui_, ui_raster_);
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
  feedback_->before_swap(artwork ? frame_serial_ : 0, swaps_ + 1);
  // Surface/context loss must terminate the host so systemd/watchdog can
  // recover it; a responsive command loop with a dead UI is not healthy.
  (void)eglGetError();
  SDL_GL_SwapWindow(window_);
  require(eglGetError()==EGL_SUCCESS,"graphics_error","Native display swap failed");
  ++swaps_;
  const auto end = std::chrono::steady_clock::now();
  swap_timing_.add(std::chrono::duration<double,std::milli>(end-start).count());
  if (changed) {
    if (last_frame_ != std::chrono::steady_clock::time_point{})
      frame_timing_.add(std::chrono::duration<double,std::milli>(end-last_frame_).count());
    last_frame_ = end; last_frame_texture_ = artwork->texture; last_frame_number_ = artwork->frames; last_frame_occurrence_ = label_occurrence_;
  }
}
bool Presentation::can_present_direct(const Occurrence& occurrence,const Playback& playback) const {
  if(!playback.visible() || playback.visible()->id!=occurrence.id ||
     !playback.desired() || playback.desired()->id!=occurrence.id ||
     !playback.overlay().is_null() || playback.service_failure() || playback.composition_pending())return false;
  auto rect=bounds(occurrence.display);
  if(rect.width!=width_ || rect.height!=height_ ||
     display_color(occurrence.display.value("background","#000000"))!="#000000")return false;
  auto now=SDL_GetTicks64();
  if(pointer_active_ && now-pointer_active_<5000)return false;
  if(!playback.runtime_notice().is_null() || !playback.presentation_failure().is_null() ||
     !playback.storage_error().empty() || playback.thermal_hold())return false;
  auto schedule=playback.state().value("scheduleOutcome",Json(nullptr));
  auto update=playback.state().value("updateOutcome",Json(nullptr));
  if((schedule.is_object() && schedule.value("outcome","")=="failed") ||
     (update.is_object() && update.value("contentBlocked",false)))return false;
  auto mode=playback.state()["settings"].value("tombstone","timed");
  return !has_tombstone(occurrence) || mode=="off" ||
         (mode=="timed" && label_occurrence_==occurrence.id && label_mode_==mode && now-label_started_>=30300);
}
bool Presentation::draw(const Surface* artwork, const Occurrence* occurrence,
                        const Playback& playback, const Surface* backdrop,
                        float backdrop_opacity, const ArtworkBlend& blend,bool external) {
  const auto now = std::chrono::steady_clock::now();
  const auto ticks = SDL_GetTicks64();
  const bool pointer_visible = pointer_active_ && ticks-pointer_active_<5000;
  const auto mode = playback.state()["settings"].value("tombstone","timed");
  const bool has_title=occurrence&&has_tombstone(*occurrence);
  if(occurrence&&blend.opacity>0&&playback.overlay().is_null()&&backdrop_opacity==0&&
     (label_occurrence_!=occurrence->id||label_mode_!=mode||label_has_title_!=has_title)){
    label_occurrence_=occurrence->id;label_mode_=mode;label_has_title_=has_title;label_started_=ticks;
  }
  const auto label_age=ticks-label_started_;
  const bool label_visible = has_title && blend.opacity>0 && (mode=="on" || (mode=="timed" && label_age<30300));
  bool overlay_panel=!playback.overlay().is_null() || playback.service_failure() || playback.thermal_hold() ||
                     !playback.presentation_failure().is_null() || !playback.storage_error().empty() ||
                     !playback.runtime_notice().is_null();
  if(external && !pointer_visible && !label_visible && !overlay_panel){
    browser_overlay_.reset();draw_key_.reset();return false;
  }
  if(!external && browser_overlay_){browser_overlay_.reset();draw_key_.reset();}
  const int label_phase=label_visible&&mode=="timed"?int(std::min<uint64_t>(label_age,30300)/16):0;
  const DrawKey key{playback.revision(), artwork?artwork->texture:0, artwork?artwork->frames:0,
    occurrence?occurrence->id:"", width_,height_,pointer_visible,label_visible,
    pointer_visible?pointer_x_:0,pointer_visible?pointer_y_:0,label_phase,backdrop?backdrop->texture:0,backdrop?backdrop->frames:0,backdrop_opacity,
    blend.outgoing?blend.outgoing->texture:0,blend.outgoing?blend.outgoing->frames:0,blend.outgoing_opacity,blend.opacity};
  // Resubmitting an unchanged texture still consumes GPU/compositor work and
  // can block the next capture. UI revisions, geometry, pointer motion/expiry
  // and timed label expiry all independently invalidate composition.
  if (draw_key_ == key || !draw_clock_.due(now)) return false;
  draw_key_ = key;
  activate();
  if(external){
    if(!browser_overlay_)browser_overlay_=std::make_unique<CageOverlay>(window_,width_,height_);
    browser_overlay_->begin();
  }
  const auto* stage = blend.stage ? blend.stage : (occurrence ? &occurrence->display : nullptr);
  auto background=display_color(stage?stage->value("background","#000000"):"#000000");
  unsigned rgb=0;
  if(background.size()==7&&background[0]=='#'&&background.find_first_not_of("0123456789abcdefABCDEF",1)==std::string::npos)
    rgb=std::stoul(background.substr(1),nullptr,16);
  glBindFramebuffer(GL_FRAMEBUFFER,0);glViewport(0,0,width_,height_);glClearColor(((rgb>>16)&255)/255.f,((rgb>>8)&255)/255.f,(rgb&255)/255.f,1);glClear(GL_COLOR_BUFFER_BIT);
  if(external){glClearColor(0,0,0,0);glClear(GL_COLOR_BUFFER_BIT);}
  // Device setup can explicitly own the screen while playback sleeps. In its
  // absence sleep is black, including recent cursor and retained error UI.
  if(playback.state()["sleeping"]==true&&playback.overlay().is_null()){
    glClearColor(0,0,0,1);glClear(GL_COLOR_BUFFER_BIT);
    swap(nullptr);return true;
  }
  glEnable(GL_BLEND); glBlendFunc(GL_ONE, GL_ONE_MINUS_SRC_ALPHA);
  if (stage && blend.outgoing && blend.outgoing->texture && blend.outgoing_opacity > 0)
    artwork_quad(*blend.outgoing, bounds(*stage), blend.outgoing_opacity);
  if (stage && artwork && artwork->texture && blend.opacity > 0)
    artwork_quad(*artwork, bounds(*stage), blend.opacity);
  if(backdrop&&backdrop->texture&&backdrop_opacity>0)artwork_quad(*backdrop,{0,0,width_,height_},backdrop_opacity);
  auto overlay = playback.overlay();
  if (!overlay.is_null()) {
    auto key="panel:"+overlay.dump()+std::to_string(width_)+":"+std::to_string(height_);
    if(key!=ui_key_)upload_ui(render_panel(overlay,width_,height_),key);
    quad(ui_,{0,0,width_,height_});
  } else if (playback.thermal_hold()) ui("Cooling down\nPlayback will resume when the device cools.","",true);
  else if(playback.service_failure()) ui("Device service unavailable\nReconnecting device controls.","",false);
  else if (playback.desired() && playback.desired()->stage == "failed") ui("Artwork could not be displayed\n"+playback.desired()->error,"",true);
  else if (playback.desired() && playback.desired()->stage != "presented" && !playback.recovery_error().empty())
    ui("Retrying artwork\n"+playback.recovery_error(),"",!artwork);
  else if (!playback.presentation_failure().is_null())
    ui("Previous artwork stopped\n"+playback.presentation_failure().value("error",""),"",!artwork);
  else if (!playback.storage_error().empty()) ui("Playback state could not be saved\n"+playback.storage_error(),"",false);
  else if (auto outcome=playback.state().value("scheduleOutcome",Json(nullptr));outcome.is_object()&&outcome.value("outcome","")=="failed")
    ui("Scheduled playlist could not start\n"+outcome["error"].value("message","Unknown scheduling failure"),"",false);
  else if (auto outcome=playback.state().value("updateOutcome",Json(nullptr));outcome.is_object()&&outcome.value("contentBlocked",false)&&(!playback.desired()||outcome.value("outcome","")=="failed"))
    ui("Updated playlist is blocked\nChange content preferences or choose another playlist","",!artwork);
  else if (!external && !artwork && playback.state()["sleeping"] == false) ui(playback.desired() ? "Loading artwork" : "Feral File\nReady for artwork","",true);
  else if((!artwork || playback.online()) && !playback.runtime_notice().is_null()) ui("Offline artwork is incomplete\n"+playback.runtime_notice().value("message","Unavailable live dependencies"),"",!artwork);
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
  if (pointer_visible && overlay.is_null()) draw_pointer();
  if(external)browser_overlay_->submit();else swap(blend.opacity>0?artwork:nullptr);return true;
}
bool Presentation::events(const std::function<void(const Json&)>& input) {
  activate();
  auto reset=reinterpret_cast<PFNGLGETGRAPHICSRESETSTATUSKHRPROC>(eglGetProcAddress("glGetGraphicsResetStatusKHR"));
  require(!reset || reset()==GL_NO_ERROR,"graphics_error","Native graphics context reset");
  require(glGetError()!=GL_CONTEXT_LOST_KHR,"graphics_error","Native graphics context lost");
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
void Presentation::browser_feedback(double elapsed_ms, bool fallback) {
  if (fallback) ++browser_feedback_fallbacks_;
  else browser_feedback_timing_.add(elapsed_ms);
}
Json Presentation::metrics() const {
  return {{"gpuCopies", gpu_copies_},
          {"cpuCopies", cpu_copies_},
          {"coalescedFrames", coalesced_frames_},
          {"directTextureImports", direct_imports_},
          {"copyMilliseconds", copy_ms_},
          {"swaps", swaps_},
          {"copyTiming", copy_timing_.json()},
          {"copyFenceLatency", copy_fence_timing_.json()},
          {"swapTiming", swap_timing_.json()},
          {"newFrameSubmissionTiming", frame_timing_.json()},
          {"browserFeedbackDelay", browser_feedback_timing_.json()},
          {"browserFeedbackFallbacks", browser_feedback_fallbacks_},
          {"waylandPresentation", feedback_->metrics()}};
}
}
