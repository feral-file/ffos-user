#include "ff/presentation.hpp"
#include "ff/overlay.hpp"
#include <EGL/eglext.h>
#include <GLES2/gl2ext.h>
#include <drm_fourcc.h>
#include <pango/pangocairo.h>
#include <qrencode.h>
#include <poll.h>
#include <chrono>
#include <iostream>
#include <cerrno>

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
std::string escape(const std::string& text) {
  auto escaped = g_markup_escape_text(text.c_str(), text.size());
  std::string result(escaped); g_free(escaped); return result;
}
std::string label(const Occurrence& o) {
  auto legacy = o.item.value("metadata", Json::object());
  auto metadata = o.item.value("inlineManifest", Json::object()).value("metadata", Json::object());
  if (o.item.contains("resolvedManifest")) metadata = o.item["resolvedManifest"].value("metadata", Json::object());
  auto title = metadata.value("title", legacy.value("title", o.item.value("title", "")));
  if (title.empty()) return {};
  std::string artists;
  auto people = metadata.value("artists", legacy.value("artists", Json::array()));
  if (people.is_array()) for (const auto& artist : people) {
    auto name = artist.is_string() ? artist.get<std::string>() : artist.value("name", "");
    if (name.empty()) continue;
    if (!artists.empty()) artists += ", "; artists += name;
  }
  std::string result = artists.empty() ? "" : escape(artists) + "\n";
  result += "<b><i>" + escape(title) + "</i></b>";
  auto exhibition = o.playlist_title;
  if (!exhibition.empty()) result += "\n" + escape(exhibition);
  auto curator = o.curator;
  if (!curator.empty()) result += "\n<span foreground='#777777'>Curated by: " + escape(curator) + "</span>";
  return result;

}
}
Presentation::Presentation(bool diagnostic_cpu) : diagnostic_cpu_(diagnostic_cpu) {
  require(SDL_Init(SDL_INIT_VIDEO | SDL_INIT_EVENTS) == 0, "display_unavailable", SDL_GetError());
  SDL_GL_SetAttribute(SDL_GL_CONTEXT_PROFILE_MASK, SDL_GL_CONTEXT_PROFILE_ES);
  SDL_GL_SetAttribute(SDL_GL_CONTEXT_MAJOR_VERSION, 3); SDL_GL_SetAttribute(SDL_GL_CONTEXT_MINOR_VERSION, 0);
  SDL_GL_SetAttribute(SDL_GL_DOUBLEBUFFER, 1);
  window_ = SDL_CreateWindow("Feral File", SDL_WINDOWPOS_UNDEFINED, SDL_WINDOWPOS_UNDEFINED, 1280, 720, SDL_WINDOW_OPENGL | SDL_WINDOW_FULLSCREEN_DESKTOP | SDL_WINDOW_ALLOW_HIGHDPI);
  require(window_ != nullptr, "display_unavailable", SDL_GetError());
  context_ = SDL_GL_CreateContext(window_); require(context_ != nullptr, "display_unavailable", SDL_GetError());
  SDL_GL_SetSwapInterval(1); SDL_GL_GetDrawableSize(window_, &width_, &height_); SDL_ShowCursor(SDL_DISABLE);
  SDL_StartTextInput();
  const char* vertex = "#version 300 es\nconst vec2 p[4]=vec2[4](vec2(-1,-1),vec2(1,-1),vec2(-1,1),vec2(1,1));out vec2 uv;void main(){gl_Position=vec4(p[gl_VertexID],0,1);uv=vec2((p[gl_VertexID].x+1.)/2.,(1.-p[gl_VertexID].y)/2.);}";
  const char* fragment = "#version 300 es\nprecision mediump float;in vec2 uv;uniform sampler2D tex;uniform float opacity;out vec4 color;void main(){color=texture(tex,uv)*opacity;}";
  auto vs = shader(GL_VERTEX_SHADER, vertex), fs = shader(GL_FRAGMENT_SHADER, fragment);
  program_ = glCreateProgram(); glAttachShader(program_, vs); glAttachShader(program_, fs); glLinkProgram(program_);
  glDeleteShader(vs); glDeleteShader(fs); GLint ok; glGetProgramiv(program_, GL_LINK_STATUS, &ok); require(ok, "graphics_error", "Composition shader link failed");
  std::cerr << "presentation driver=" << SDL_GetCurrentVideoDriver() << " renderer=" << glGetString(GL_RENDERER) << " size=" << width_ << 'x' << height_ << '\n';
}
Presentation::~Presentation() {
  glDeleteTextures(1, &ui_); glDeleteProgram(program_);
  if (context_) SDL_GL_DeleteContext(context_); if (window_) SDL_DestroyWindow(window_); SDL_Quit();
}
void Presentation::destroy(Surface& surface) { glDeleteTextures(1, &surface.texture); surface = {}; }
bool Presentation::accelerated(Surface& surface, const CefAcceleratedPaintInfo& info) {
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
    // Borrowed buffers cannot outlive the callback. Skip a busy producer and
    // request another paint instead of blocking the command loop or declaring
    // a temporary GPU wait terminal. No handle or stale contents are retained.
    // This still cannot repair missing producer fences on CEF #4313/NVIDIA.
    pollfd fd{info.planes[i].fd, POLLIN, 0};
    int result=::poll(&fd,1,0);
    if(result==0||(result<0&&errno==EINTR)){++producer_busy_frames_;return false;}
    require(result>0&&(fd.revents&POLLIN),"graphics_error","DMA-BUF producer fence is invalid");
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
  glFinish(); auto error = glGetError(); glBindFramebuffer(GL_FRAMEBUFFER, 0);
  glDeleteFramebuffers(2, framebuffers); glDeleteTextures(1, &imported); release(display, image);
  require(source_ok && error == GL_NO_ERROR, "graphics_error", "DMA-BUF GPU copy failed");
  surface.accelerated = true; ++surface.frames; ++gpu_copies_;
  copy_ms_ += std::chrono::duration<double, std::milli>(std::chrono::steady_clock::now() - started).count();
  return true;
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
void Presentation::ui(const std::string& text, const std::string& qr, bool full) {
  std::string key = text + '\n' + qr + (full ? "full" : "label");
  if (key != ui_key_) {
    ui_key_ = key;
    auto measuring = cairo_image_surface_create(CAIRO_FORMAT_ARGB32, 1, 1);
    auto measure_cr = cairo_create(measuring);
    auto layout = pango_cairo_create_layout(measure_cr);
    auto font = pango_font_description_from_string(full ? "Sans 22" : "PP Mori 16");
    pango_layout_set_font_description(layout, font);
    pango_layout_set_width(layout, (full ? 928 : 620) * PANGO_SCALE);
    pango_layout_set_wrap(layout, PANGO_WRAP_WORD_CHAR);
    pango_layout_set_markup(layout, text.c_str(), static_cast<int>(text.size()));
    if(full){pango_layout_set_height(layout,480*PANGO_SCALE);pango_layout_set_ellipsize(layout,PANGO_ELLIPSIZE_END);}
    int text_width, text_height; pango_layout_get_pixel_size(layout, &text_width, &text_height);
    ui_width_ = full ? 1024 : std::min(640, text_width + 20);
    ui_height_ = full ? std::max(768,96+text_height+(qr.empty()?0:348)) : std::min(700,text_height + 20);
    auto surface = cairo_image_surface_create(CAIRO_FORMAT_ARGB32, ui_width_, ui_height_);
    auto cr = cairo_create(surface);
    cairo_set_source_rgb(cr, full ? 0 : 1, full ? 0 : 1, full ? 0 : 1); cairo_paint(cr);
    cairo_set_source_rgb(cr, full ? 1 : 0, full ? 1 : 0, full ? 1 : 0);
    cairo_move_to(cr, full ? 48 : 10, full ? 48 : 10); pango_cairo_show_layout(cr, layout);
    cairo_destroy(measure_cr); cairo_surface_destroy(measuring);
    if (!qr.empty()) {
      auto code = QRcode_encodeString(qr.c_str(), 0, QR_ECLEVEL_M, QR_MODE_8, 1);
      if (code) {
        const double cell = 300.0 / (code->width + 8); cairo_set_source_rgb(cr, 1, 1, 1); cairo_rectangle(cr, 362, 72+text_height, 300, 300); cairo_fill(cr);
        cairo_set_source_rgb(cr, 0, 0, 0);
        for (int y = 0; y < code->width; ++y) for (int x = 0; x < code->width; ++x) if (code->data[y*code->width+x] & 1) { cairo_rectangle(cr, 362 + (x+4)*cell, 72+text_height+(y+4)*cell, cell+0.1, cell+0.1); cairo_fill(cr); }
        QRcode_free(code);
      }
    }
    cairo_surface_flush(surface);
    auto data = cairo_image_surface_get_data(surface); std::vector<unsigned char> pixels(static_cast<size_t>(ui_width_)*ui_height_*4);
    for (size_t i=0;i<pixels.size();i+=4) { pixels[i]=data[i+2];pixels[i+1]=data[i+1];pixels[i+2]=data[i];pixels[i+3]=data[i+3]; }
    if (!ui_) glGenTextures(1,&ui_); glBindTexture(GL_TEXTURE_2D,ui_);
    glTexImage2D(GL_TEXTURE_2D,0,GL_RGBA,ui_width_,ui_height_,0,GL_RGBA,GL_UNSIGNED_BYTE,pixels.data());
    glTexParameteri(GL_TEXTURE_2D,GL_TEXTURE_MIN_FILTER,GL_LINEAR);glTexParameteri(GL_TEXTURE_2D,GL_TEXTURE_MAG_FILTER,GL_LINEAR);
    pango_font_description_free(font); g_object_unref(layout); cairo_destroy(cr); cairo_surface_destroy(surface);
  }
  if (full) {
    // Preserve QR squares and glyph proportions on portrait/square displays.
    glClearColor(0,0,0,1);glClear(GL_COLOR_BUFFER_BIT);
    double scale=std::min(width_/double(ui_width_),height_/double(ui_height_));
    int w=ui_width_*scale,h=ui_height_*scale;quad(ui_,{(width_-w)/2,(height_-h)/2,w,h});
  }
  else { double scale=std::min(width_,height_)/720.0; int w=ui_width_*scale,h=ui_height_*scale;quad(ui_,{0,height_-h,w,h}); }
}
void Presentation::draw(const Surface* artwork, const Occurrence* occurrence, const Playback& playback) {
  require(SDL_GL_MakeCurrent(window_, context_) == 0, "graphics_error", SDL_GetError());
  glBindFramebuffer(GL_FRAMEBUFFER,0);glViewport(0,0,width_,height_);glClearColor(0,0,0,1);glClear(GL_COLOR_BUFFER_BIT);
  // Device setup can explicitly own the screen while playback sleeps. In its
  // absence sleep is black, including recent cursor and retained error UI.
  if(playback.state()["sleeping"]==true&&playback.overlay().is_null()){
    SDL_GL_SwapWindow(window_);++swaps_;return;
  }
  glEnable(GL_BLEND); glBlendFunc(GL_ONE, GL_ONE_MINUS_SRC_ALPHA);
  if (artwork && artwork->texture && occurrence) quad(artwork->texture,bounds(occurrence->display));
  if (occurrence && label_occurrence_ != occurrence->id) { label_occurrence_ = occurrence->id; label_started_ = SDL_GetTicks64(); }
  auto overlay = playback.overlay();
  if (!overlay.is_null()) {
    auto panel=overlay_panel(overlay);
    ui(escape(panel.text),panel.qr,true);
  } else if (playback.desired() && playback.desired()->stage == "failed") ui("Artwork could not be displayed\n"+escape(playback.desired()->error),"",true);
  else if (playback.desired() && playback.desired()->stage != "presented" && !playback.recovery_error().empty())
    ui("Retrying artwork\n"+escape(playback.recovery_error()),"",!artwork);
  else if (!playback.presentation_failure().is_null())
    ui("Previous artwork stopped\n"+escape(playback.presentation_failure().value("error","")),"",!artwork);
  else if (!playback.storage_error().empty()) ui("Playback state could not be saved\n"+escape(playback.storage_error()),"",false);
  else if (auto outcome=playback.state().value("scheduleOutcome",Json(nullptr));outcome.is_object()&&outcome.value("outcome","")=="failed")
    ui("Scheduled playlist could not start\n"+escape(outcome["error"].value("message","Unknown scheduling failure")),"",false);
  else if (auto outcome=playback.state().value("updateOutcome",Json(nullptr));outcome.is_object()&&outcome.value("contentBlocked",false)&&(!playback.desired()||outcome.value("outcome","")=="failed"))
    ui("Updated playlist is blocked\nChange content preferences or choose another playlist","",!artwork);
  else if (!artwork && playback.state()["sleeping"] == false) ui(playback.desired() ? "Loading artwork" : "Feral File\nReady for artwork","",true);
  else if (occurrence) {
    auto mode = playback.state()["settings"].value("tombstone","timed");
    if (mode == "on" || (mode == "timed" && SDL_GetTicks64() - label_started_ < 30000)) { auto text=label(*occurrence); if(!text.empty())ui(text,"",false); }
  }
  if (pointer_active_ && SDL_GetTicks64()-pointer_active_<5000 && overlay.is_null()) {
    glEnable(GL_SCISSOR_TEST);glScissor(static_cast<int>(pointer_x_)-5,height_-static_cast<int>(pointer_y_)-5,10,10);glClearColor(1,1,1,1);glClear(GL_COLOR_BUFFER_BIT);glDisable(GL_SCISSOR_TEST);
  }
  SDL_GL_SwapWindow(window_); ++swaps_;
}
bool Presentation::events(const std::function<void(const Json&)>& input) {
  SDL_Event event;
  while(SDL_PollEvent(&event)) {
    if(event.type==SDL_QUIT)return false;
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
  return true;
}
Json Presentation::metrics() const { return {{"gpuCopies",gpu_copies_},{"cpuCopies",cpu_copies_},{"producerBusyFrames",producer_busy_frames_},{"copyMilliseconds",copy_ms_},{"swaps",swaps_}}; }
}
