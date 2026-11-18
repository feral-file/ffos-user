#include "ff/cef_client.hpp"
#include "include/cef_parser.h"
#include "include/cef_process_message.h"
#include <algorithm>
#include <cmath>
#include <iostream>

namespace ff {
ArtworkClient::ArtworkClient(Playback& playback, Presentation& presentation, Occurrence occurrence, bool is_bundled, bool is_decoration)
    : id(occurrence.id), bundled(is_bundled), decoration(is_decoration), playback_(playback), presentation_(presentation), occurrence_(std::move(occurrence)) {}
void ArtworkClient::failed(const std::string& error) {
  if(decoration){decoration_failed=true;ready=false;std::cerr<<"Setup backdrop failed: "<<error<<'\n';}
  else playback_.failed(id,error);
}
bool ArtworkClient::OnConsoleMessage(CefRefPtr<CefBrowser>,cef_log_severity_t severity,const CefString& message,const CefString&,int){
  if(bundled&&severity>=LOGSEVERITY_WARNING)std::cerr<<"renderer "<<id<<": "<<message.ToString().substr(0,1024)<<'\n';
  return false;
}
void ArtworkClient::OnAfterCreated(CefRefPtr<CefBrowser> b) {
  browser=b;browser->GetHost()->SetFocus(false);
  if(closing)browser->GetHost()->CloseBrowser(true);
}
void ArtworkClient::OnBeforeClose(CefRefPtr<CefBrowser>) { browser=nullptr;closed=true;presentation_.destroy(surface); }
bool ArtworkClient::OnBeforeBrowse(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame> frame, CefRefPtr<CefRequest> request, bool, bool) {
  auto url=request->GetURL().ToString();
  if(decoration)return !frame->IsMain()||url!="ff-render://bundle/setup-artwork/index.html";
  if(bundled && frame->IsMain())return url != "ff-render://bundle/index.html";
  return !(url.starts_with("https://")||url.starts_with("http://")||url.starts_with("about:blank")||url.starts_with("blob:")||url.starts_with("data:"));
}
void ArtworkClient::OnLoadEnd(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame> frame, int status) {
  if(!frame->IsMain())return;
  if(status>=400){failed("HTTP "+std::to_string(status));return;}
  if(!bundled)mark_ready();
}
void ArtworkClient::OnLoadError(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame> frame, ErrorCode code, const CefString& text, const CefString&) {
  if(frame->IsMain()&&code!=ERR_ABORTED)failed("Navigation failed: "+text.ToString());
}
void ArtworkClient::GetViewRect(CefRefPtr<CefBrowser>, CefRect& rect) {
  auto b=presentation_.bounds(occurrence_.display);rect={0,0,b.width,b.height};view_width_=b.width;view_height_=b.height;
}
bool ArtworkClient::admit_paint(){
  if(closing)return false;
  // CEF can deliver several capture callbacks before the host gets its next
  // turn. Copy only one: the others cannot be presented and waiting on all of
  // their producer fences delays commands. Retain no borrowed handle/pixels.
  if(paint_pending_){refresh_needed_=true;presentation_.coalesced_frame();return false;}
  paint_pending_=true;return true;
}
void ArtworkClient::OnPaint(CefRefPtr<CefBrowser>, PaintElementType type, const RectList&,const void* data,int width,int height) {
  if(type!=PET_VIEW||!admit_paint())return;
  try {presentation_.software(surface,data,width,height);}catch(const std::exception& e){failed(e.what());}
}
void ArtworkClient::OnAcceleratedPaint(CefRefPtr<CefBrowser>, PaintElementType type,const RectList&,const CefAcceleratedPaintInfo& info) {
  if(type!=PET_VIEW||!admit_paint())return;
  try {presentation_.accelerated(surface,info);}
  catch(const std::exception& e){failed(e.what());}
}
void ArtworkClient::OnRenderProcessTerminated(CefRefPtr<CefBrowser>, TerminationStatus,int code,const CefString& detail) {
  ready=false;failed("Renderer terminated ("+std::to_string(code)+"): "+detail.ToString());
}
bool ArtworkClient::OnRenderProcessUnresponsive(CefRefPtr<CefBrowser>,CefRefPtr<CefUnresponsiveProcessCallback> callback) {
  failed("Artwork renderer is unresponsive");callback->Terminate();return true;
}
void ArtworkClient::close(){if(!closing){closing=true;if(browser)browser->GetHost()->CloseBrowser(true);}}
void ArtworkClient::mark_ready(){
  if(ready)return;
  if(!decoration)playback_.committed(id);ready_after_frame_=surface.frames;ready=true;
  // Pre-load frames (including the bundled viewer's empty canvas) are not
  // admission evidence. Request a post-ready frame even for static artworks.
  if(browser)browser->GetHost()->Invalidate(PET_VIEW);
}
void ArtworkClient::configure(){
  if(!browser||!bundled)return;
  Json config={{"source",occurrence_.item["source"]},{"display",occurrence_.display},{"mime",occurrence_.item.value("mimeType","")},{"paused",playback_.state()["paused"]},{"online",playback_.online()}, {"volume",playback_.state()["settings"].value("volume",1.0)},{"muted",playback_.state()["settings"].value("muted",false)}};
  auto message=CefProcessMessage::Create("renderer.configure");message->GetArgumentList()->SetString(0,config.dump());browser->GetMainFrame()->SendProcessMessage(PID_RENDERER,message);
}
void ArtworkClient::update_media(){
  paint_pending_=false;
  // Pace Chromium from host progress rather than its independent 60 Hz timer.
  // Otherwise heavy WebGL can build a long producer queue while this UI thread
  // waits for a copy; coalescing callbacks alone does not drain that GPU backlog.
  const auto now=std::chrono::steady_clock::now();
  if(browser && begin_clock_.due(now)){
    browser->GetHost()->SendExternalBeginFrame();
  }
  // A dropped post-load frame may have been a static document's only update.
  // Request a replacement once per host turn, never recursively in OnPaint.
  if(refresh_needed_&&browser){refresh_needed_=false;browser->GetHost()->Invalidate(PET_VIEW);}

  if(playback_.desired()&&playback_.desired()->id==id)occurrence_.display=playback_.desired()->display;
  else if(playback_.visible()&&playback_.visible()->id==id)occurrence_.display=playback_.visible()->display;
  auto bounds=presentation_.bounds(occurrence_.display);
  if(browser&&(view_width_!=bounds.width||view_height_!=bounds.height))browser->GetHost()->WasResized();
  auto key=occurrence_.display.dump()+playback_.state()["settings"].dump()+playback_.state()["paused"].dump()+(playback_.online()?"on":"off");
  if(key!=media_state_){media_state_=key;configure();}
}
bool ArtworkClient::OnProcessMessageReceived(CefRefPtr<CefBrowser>,CefRefPtr<CefFrame> frame,CefProcessId source,CefRefPtr<CefProcessMessage> message){
  // Trusted capability is scoped to the bundled main document, not merely a
  // browser ID. Navigating an artist page cannot acquire this bridge.
  if(!bundled||source!=PID_RENDERER||!frame->IsMain()||frame->GetURL()!="ff-render://bundle/index.html")return false;
  auto name=message->GetName();
  if(name=="renderer.configure"){configure();return true;}
  if(name=="renderer.ready"){mark_ready();return true;}
  if(name=="renderer.ended"){playback_.ended(id);return true;}
  if(name=="renderer.error"){failed(message->GetArgumentList()->GetString(0).ToString().substr(0,512));return true;}
  return false;
}
void ArtworkClient::input(const Json& p){
  if(!browser)return;
  auto host=browser->GetHost();auto kind=string_field(p,"kind",32);
  if(kind=="blur"){host->SetFocus(false);return;}
  host->SetFocus(true);
  uint32_t modifiers=0,buttons=p.value("buttons",0U);
  if(buttons&1)modifiers|=EVENTFLAG_LEFT_MOUSE_BUTTON;
  if(buttons&2)modifiers|=EVENTFLAG_MIDDLE_MOUSE_BUTTON;
  if(buttons&4)modifiers|=EVENTFLAG_RIGHT_MOUSE_BUTTON;
  if(p.value("shift",false))modifiers|=EVENTFLAG_SHIFT_DOWN;
  if(p.value("control",false))modifiers|=EVENTFLAG_CONTROL_DOWN;
  if(p.value("alt",false))modifiers|=EVENTFLAG_ALT_DOWN;
  if(p.value("meta",false))modifiers|=EVENTFLAG_COMMAND_DOWN;
  CefMouseEvent mouse;mouse.x=p.value("x",0);mouse.y=p.value("y",0);mouse.modifiers=modifiers;
  if(kind=="move")host->SendMouseMoveEvent(mouse,p.value("outside",false)&&buttons==0);
  else if(kind=="button"){
    int button=p.at("button").get<int>();
    host->SendMouseClickEvent(mouse,button==1?MBT_LEFT:button==2?MBT_MIDDLE:MBT_RIGHT,!p.at("down").get<bool>(),p.value("clicks",1));
  }else if(kind=="wheel")host->SendMouseWheelEvent(mouse,p.value("dx",0),p.value("dy",0));
  else if(kind=="key"){
    int usage=p.at("usage").get<int>();auto key=physical_key(usage);
    CefKeyEvent event;event.type=p.at("down").get<bool>()?KEYEVENT_RAWKEYDOWN:KEYEVENT_KEYUP;
    event.windows_key_code=key.virtual_key;event.native_key_code=key.native_key;event.modifiers=modifiers;
    if(p.value("repeat",false))event.modifiers|=EVENTFLAG_IS_REPEAT;
    if(usage>=224&&usage<=227)event.modifiers|=EVENTFLAG_IS_LEFT;
    if(usage>=228&&usage<=231)event.modifiers|=EVENTFLAG_IS_RIGHT;
    if(usage>=83&&usage<=99)event.modifiers|=EVENTFLAG_IS_KEY_PAD;
    host->SendKeyEvent(event);
  }else if(kind=="text"){
    CefString wide(string_field(p,"text",4096));
    for(auto character:wide.ToString16()){
      CefKeyEvent event;event.type=KEYEVENT_CHAR;event.character=character;event.unmodified_character=character;event.modifiers=modifiers;host->SendKeyEvent(event);
    }
  }
}

std::string renderer_url(const Occurrence& o){
  auto source=o.item["source"].get<std::string>();auto mime=o.item.value("mimeType","");
  auto path=source.substr(0,source.find_first_of("?#"));std::transform(path.begin(),path.end(),path.begin(),[](unsigned char c){return std::tolower(c);});
  for(auto extension:{".jpg",".jpeg",".png",".gif",".webp",".avif",".svg",".glb",".gltf"})if(path.ends_with(extension))return "ff-render://bundle/index.html";
  if(mime.starts_with("image/")||mime.starts_with("model/"))return "ff-render://bundle/index.html";
  return source;
}
}
