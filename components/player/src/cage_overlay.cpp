#include "ff/cage_overlay.hpp"
#include "ff/json.hpp"
#include "xdg-shell-client-protocol.h"
#include <SDL_syswm.h>
#include <EGL/egl.h>
#include <wayland-egl.h>
#include <cstring>

namespace ff {
struct CageOverlay::Impl {
  wl_display* display=nullptr;
  wl_event_queue* queue=nullptr;
  wl_registry* registry=nullptr;
  wl_compositor* compositor=nullptr;
  xdg_wm_base* shell=nullptr;
  wl_surface* surface=nullptr;
  xdg_surface* xsurface=nullptr;
  xdg_toplevel* top=nullptr;
  wl_egl_window* window=nullptr;
  EGLDisplay egl=EGL_NO_DISPLAY;
  EGLContext owner=EGL_NO_CONTEXT,context=EGL_NO_CONTEXT;
  EGLSurface original=EGL_NO_SURFACE,layer=EGL_NO_SURFACE;
  bool configured=false;
  ~Impl(){
    if(egl!=EGL_NO_DISPLAY){
      if(eglGetCurrentContext()==context)eglMakeCurrent(egl,original,original,owner);
      if(layer!=EGL_NO_SURFACE)eglDestroySurface(egl,layer);
      if(context!=EGL_NO_CONTEXT)eglDestroyContext(egl,context);
    }
    if(window)wl_egl_window_destroy(window);
    if(top)xdg_toplevel_destroy(top);
    if(xsurface)xdg_surface_destroy(xsurface);
    if(surface)wl_surface_destroy(surface);
    if(shell)xdg_wm_base_destroy(shell);
    if(compositor)wl_compositor_destroy(compositor);
    if(registry)wl_registry_destroy(registry);
    if(queue)wl_event_queue_destroy(queue);
    if(display)wl_display_flush(display);
  }
};
CageOverlay::CageOverlay(SDL_Window* owner,int width,int height):impl_(std::make_unique<Impl>()){
  auto& s=*impl_;
  SDL_SysWMinfo wm{};SDL_VERSION(&wm.version);
  require(SDL_GetWindowWMInfo(owner,&wm)&&wm.subsystem==SDL_SYSWM_WAYLAND,"device_ui","Wayland required");
  s.display=wm.info.wl.display;
  s.queue=wl_display_create_queue(s.display);
  require(s.queue,"device_ui","Cannot create overlay queue");
  auto* wrapper=static_cast<wl_display*>(wl_proxy_create_wrapper(s.display));
  require(wrapper,"device_ui","Cannot create display wrapper");
  wl_proxy_set_queue(reinterpret_cast<wl_proxy*>(wrapper),s.queue);
  s.registry=wl_display_get_registry(wrapper); wl_proxy_wrapper_destroy(wrapper);
  static const wl_registry_listener globals={
    [](void* data,wl_registry* reg,uint32_t name,const char* iface,uint32_t version){
      auto& self=*static_cast<Impl*>(data);
      if(!strcmp(iface,"wl_compositor"))
        self.compositor=static_cast<wl_compositor*>(wl_registry_bind(reg,name,&wl_compositor_interface,std::min(version,4u)));
      if(!strcmp(iface,"xdg_wm_base"))
        self.shell=static_cast<xdg_wm_base*>(wl_registry_bind(reg,name,&xdg_wm_base_interface,1));
    },[](void*,wl_registry*,uint32_t){}};
  wl_registry_add_listener(s.registry,&globals,&s);
  require(wl_display_roundtrip_queue(s.display,s.queue)>=0&&s.compositor&&s.shell,"device_ui","Missing Cage shell");
  static const xdg_wm_base_listener ping={[](void*,xdg_wm_base* shell,uint32_t id){xdg_wm_base_pong(shell,id);}};
  xdg_wm_base_add_listener(s.shell,&ping,nullptr);
  s.surface=wl_compositor_create_surface(s.compositor);
  auto* region=wl_compositor_create_region(s.compositor);wl_surface_set_input_region(s.surface,region);wl_region_destroy(region);
  s.xsurface=xdg_wm_base_get_xdg_surface(s.shell,s.surface);
  static const xdg_surface_listener configure={[](void* data,xdg_surface* surface,uint32_t serial){
    xdg_surface_ack_configure(surface,serial);static_cast<Impl*>(data)->configured=true;
  }};
  xdg_surface_add_listener(s.xsurface,&configure,&s);
  s.top=xdg_surface_get_toplevel(s.xsurface);
  static const xdg_toplevel_listener toplevel={
    [](void*,xdg_toplevel*,int32_t,int32_t,wl_array*){},[](void*,xdg_toplevel*){}};
  xdg_toplevel_add_listener(s.top,&toplevel,nullptr);
  xdg_toplevel_set_title(s.top,"FF1 device controls");xdg_toplevel_set_fullscreen(s.top,nullptr);
  wl_surface_commit(s.surface);
  require(wl_display_roundtrip_queue(s.display,s.queue)>=0&&s.configured,"device_ui","Cage did not configure overlay");
  s.egl=eglGetCurrentDisplay();s.owner=eglGetCurrentContext();s.original=eglGetCurrentSurface(EGL_DRAW);
  EGLint attrs[]={EGL_SURFACE_TYPE,EGL_WINDOW_BIT,EGL_RENDERABLE_TYPE,EGL_OPENGL_ES3_BIT,
    EGL_RED_SIZE,8,EGL_GREEN_SIZE,8,EGL_BLUE_SIZE,8,EGL_ALPHA_SIZE,8,EGL_NONE};
  EGLint count=0;EGLConfig config{};
  require(eglChooseConfig(s.egl,attrs,&config,1,&count)&&count,"device_ui","No transparent EGL config");
  EGLint params[]={EGL_CONTEXT_CLIENT_VERSION,3,EGL_NONE};
  s.context=eglCreateContext(s.egl,config,s.owner,params);
  s.window=wl_egl_window_create(s.surface,width,height);
  require(s.context!=EGL_NO_CONTEXT&&s.window,"device_ui","Cannot create overlay window");
  s.layer=eglCreateWindowSurface(s.egl,config,reinterpret_cast<EGLNativeWindowType>(s.window),nullptr);
  require(s.layer!=EGL_NO_SURFACE,"device_ui","Cannot allocate overlay surface");
}
CageOverlay::~CageOverlay()=default;
void CageOverlay::begin(){
  auto& s=*impl_;
  wl_display_dispatch_queue_pending(s.display,s.queue);
  require(eglMakeCurrent(s.egl,s.layer,s.layer,s.context),"device_ui","Cannot activate overlay");
  eglSwapInterval(s.egl,0);
}
void CageOverlay::submit(){
  auto& s=*impl_;
  require(eglSwapBuffers(s.egl,s.layer),"device_ui","Cannot submit device overlay");
  require(eglMakeCurrent(s.egl,s.original,s.original,s.owner),"device_ui","Cannot restore device UI context");
}
}
