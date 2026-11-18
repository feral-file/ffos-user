#include "ff/cef_client.hpp"
#include "include/cef_parser.h"
#include "include/cef_scheme.h"
#include "include/cef_v8.h"
#include "include/wrapper/cef_stream_resource_handler.h"
#include <filesystem>
#include <iostream>

namespace ff {
namespace {
bool bundled(CefRefPtr<CefFrame> frame){return frame&&frame->IsMain()&&frame->GetURL()=="ff-render://bundle/index.html";}
class Emit : public CefV8Handler {
 public:
  bool Execute(const CefString&,CefRefPtr<CefV8Value>,const CefV8ValueList& args,CefRefPtr<CefV8Value>&,CefString& exception)override{
    auto context=CefV8Context::GetCurrentContext();
    if(!context||!bundled(context->GetFrame())||args.empty()||!args[0]->IsString()){exception="Renderer bridge refused";return true;}
    auto kind=args[0]->GetStringValue().ToString();
    if(kind!="configure"&&kind!="ready"&&kind!="ended"&&kind!="error"){exception="Unknown renderer observation";return true;}
    auto message=CefProcessMessage::Create("renderer."+kind);
    if(args.size()>1&&args[1]->IsString())message->GetArgumentList()->SetString(0,args[1]->GetStringValue().ToString().substr(0,512));
    context->GetFrame()->SendProcessMessage(PID_BROWSER,message);return true;
  }
  IMPLEMENT_REFCOUNTING(Emit);
};
class Resources : public CefSchemeHandlerFactory {
 public:
  explicit Resources(std::string path):path_(std::move(path)){}
  CefRefPtr<CefResourceHandler> Create(CefRefPtr<CefBrowser>,CefRefPtr<CefFrame>,const CefString&,CefRefPtr<CefRequest> request)override{
    CefURLParts parts;if(!CefParseURL(request->GetURL(),parts))return nullptr;
    auto path=CefString(&parts.path).ToString();
    if(CefString(&parts.host)!="bundle"||path.empty()||path.find("..")!=std::string::npos||path.find('%')!=std::string::npos)return nullptr;
    auto file=std::filesystem::path(path_)/path.substr(1);
    if(!std::filesystem::is_regular_file(file))return nullptr;
    auto stream=CefStreamReader::CreateForFile(file.string());if(!stream)return nullptr;
    if(path.starts_with("/draco/")||path.starts_with("/basis/"))std::cerr<<"bundled_decoder "<<path<<'\n';
    std::string mime="application/octet-stream";auto ext=file.extension().string();
    if(ext==".html")mime="text/html";else if(ext==".js")mime="text/javascript";else if(ext==".wasm")mime="application/wasm";
    CefResponse::HeaderMap headers;
    // Only the bundled renderer receives this policy. The exact Basis decoder
    // used by model-viewer 4.3.1 generates embind wrappers with new Function in
    // its blob worker, requiring unsafe-eval as well as WASM compilation.
    // Scripts still come only from this pinned bundle. Artist responses retain
    // their own CSP and receive no privileged bridge.
    if(path.starts_with("/setup-artwork/"))
      headers.emplace("Content-Security-Policy","default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'none'; object-src 'none'; frame-src 'none'; base-uri 'none'");
    else headers.emplace("Content-Security-Policy","default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src https: http: data: blob:; media-src https: http: data: blob:; connect-src 'self' https: http: data: blob:; worker-src 'self' blob:; object-src 'none'; frame-src 'none'; base-uri 'none'");
    headers.emplace("X-Content-Type-Options","nosniff");
    return new CefStreamResourceHandler(200,"OK",mime,headers,stream);
  }
 private:
  std::string path_;
  IMPLEMENT_REFCOUNTING(Resources);
};
class App : public CefApp,public CefRenderProcessHandler {
 public:
  CefRefPtr<CefRenderProcessHandler> GetRenderProcessHandler()override{return this;}
  void OnRegisterCustomSchemes(CefRawPtr<CefSchemeRegistrar> registrar)override{
    registrar->AddCustomScheme("ff-render",CEF_SCHEME_OPTION_STANDARD|CEF_SCHEME_OPTION_SECURE|CEF_SCHEME_OPTION_CORS_ENABLED|CEF_SCHEME_OPTION_FETCH_ENABLED);
  }
  void OnBeforeCommandLineProcessing(const CefString& process,CefRefPtr<CefCommandLine> command)override{
    if(process.empty()){
      if(!command->HasSwitch("ozone-platform"))command->AppendSwitchWithValue("ozone-platform","wayland");
      // Chromium 156's GL backend drops p5 WebGL line geometry on FFOS AMD.
      // ANGLE Vulkan renders the same unmodified work in CEF and Chrome;
      // ENGINE.md records the regression suite and remaining qualification.
      if(!command->HasSwitch("use-angle"))command->AppendSwitchWithValue("use-angle","vulkan");
      command->AppendSwitchWithValue("autoplay-policy","no-user-gesture-required");
    }
  }
  void OnContextCreated(CefRefPtr<CefBrowser>,CefRefPtr<CefFrame> frame,CefRefPtr<CefV8Context> context)override{
    if(bundled(frame))context->GetGlobal()->SetValue("ffRenderer",CefV8Value::CreateFunction("ffRenderer",new Emit),static_cast<cef_v8_propertyattribute_t>(V8_PROPERTY_ATTRIBUTE_READONLY|V8_PROPERTY_ATTRIBUTE_DONTDELETE));
  }
  bool OnProcessMessageReceived(CefRefPtr<CefBrowser>,CefRefPtr<CefFrame> frame,CefProcessId source,CefRefPtr<CefProcessMessage> message)override{
    if(source!=PID_BROWSER||!bundled(frame)||message->GetName()!="renderer.configure")return false;
    auto context=frame->GetV8Context();if(!context||!context->Enter())return true;
    auto fn=context->GetGlobal()->GetValue("ffConfigure");
    if(fn&&fn->IsFunction())fn->ExecuteFunction(nullptr,{CefV8Value::CreateString(message->GetArgumentList()->GetString(0))});
    context->Exit();return true;
  }
 private:
  IMPLEMENT_REFCOUNTING(App);
};
}
CefRefPtr<CefApp> renderer_app(){return new App;}
void register_renderer_resources(CefRefPtr<CefRequestContext> context,const std::string& resources){
  // Global registration does not apply to isolated request contexts. Only
  // bundled media contexts receive this factory; artist contexts never do.
  require(context->RegisterSchemeHandlerFactory("ff-render","bundle",new Resources(resources)),
    "renderer_resources","Cannot register bundled renderer resources");
}
}
