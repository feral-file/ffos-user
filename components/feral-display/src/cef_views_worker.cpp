// Single native Wayland browser view. The FF1 host owns navigation over its
// inherited engine pipes. A probe profile loads the fixed benchmark directly.
#include "include/cef_app.h"
#include "capture_stream.hpp"
#include "include/cef_client.h"
#include "include/cef_command_line.h"
#include "include/cef_task.h"
#include "include/views/cef_browser_view.h"
#include "include/views/cef_browser_view_delegate.h"
#include "include/views/cef_window.h"
#include "include/views/cef_fill_layout.h"
#include "include/views/cef_window_delegate.h"
#include <csignal>
#include <fcntl.h>
#include <sys/socket.h>
#include <unistd.h>
#include <filesystem>
#include <iostream>
#include <sys/stat.h>

namespace {
volatile std::sig_atomic_t stopping=0;
void stop(int){stopping=1;}
class Client final : public CefClient,public CefLifeSpanHandler {
 public:
  CefRefPtr<CefLifeSpanHandler> GetLifeSpanHandler() override{return this;}
  // This process owns one artwork. A popup must never take the display or
  // trigger the main window's shutdown path when the host dismisses it.
  bool OnBeforePopup(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame>, int, const CefString&,
                     const CefString&, cef_window_open_disposition_t, bool,
                     const CefPopupFeatures&, CefWindowInfo&, CefRefPtr<CefClient>&,
                     CefBrowserSettings&, CefRefPtr<CefDictionaryValue>&, bool*) override {
    return true;
  }
  void OnBeforeClose(CefRefPtr<CefBrowser>) override{CefQuitMessageLoop();}
 private: IMPLEMENT_REFCOUNTING(Client);
};
class BrowserDelegate final : public CefBrowserViewDelegate {
 public:
  cef_runtime_style_t GetBrowserRuntimeStyle() override{return CEF_RUNTIME_STYLE_ALLOY;}
 private: IMPLEMENT_REFCOUNTING(BrowserDelegate);
};
class WindowDelegate final : public CefWindowDelegate {
 public:
  explicit WindowDelegate(CefRefPtr<CefBrowserView> view):view_(view){}
  void OnWindowCreated(CefRefPtr<CefWindow> window) override{
    window->SetToFillLayout();window->AddChildView(view_);window->SetTitle("Feral File artwork");
    window->SetFullscreen(true);
    if(!CefCommandLine::GetGlobalCommandLine()->HasSwitch("ff-direct-cage"))window->Show();
  }
  void OnWindowDestroyed(CefRefPtr<CefWindow>) override{view_=nullptr;}
  bool CanClose(CefRefPtr<CefWindow>) override{
    auto browser=view_?view_->GetBrowser():nullptr;
    return !browser||browser->GetHost()->TryCloseBrowser();
  }
  bool IsFrameless(CefRefPtr<CefWindow>) override{return true;}
  cef_runtime_style_t GetWindowRuntimeStyle() override{return CEF_RUNTIME_STYLE_ALLOY;}
 private:
  CefRefPtr<CefBrowserView> view_;
  IMPLEMENT_REFCOUNTING(WindowDelegate);
};
class ShutdownTask final : public CefTask {
 public:
  ShutdownTask(CefRefPtr<CefWindow> window,bool pending):window_(window),pending_(pending){}
  void Execute() override{
    if(stopping){window_->Close();window_=nullptr;return;}
    if(pending_){
      char show=0;
      if(recv(6,&show,1,MSG_DONTWAIT)==1 && show==1){
        window_->Show();
        pending_=false;
        close(6);
      }
    }
    CefPostDelayedTask(TID_UI,this,50);
  }
 private:
  CefRefPtr<CefWindow> window_;
  bool pending_;
  IMPLEMENT_REFCOUNTING(ShutdownTask);
};
class App final : public CefApp,public CefBrowserProcessHandler {
 public:
  CefRefPtr<CefBrowserProcessHandler> GetBrowserProcessHandler() override{return this;}
  void OnContextInitialized() override{
    std::ifstream index("/streams/index.json");
    if (index) {
      CefRefPtr<CaptureFactory> factory = new CaptureFactory(nlohmann::json::parse(index));
      auto context = CefRequestContext::GetGlobalContext();
      if (!context->RegisterSchemeHandlerFactory("https", "", factory) ||
          !context->RegisterSchemeHandlerFactory("http", "", factory)) {
        std::cerr << "Cannot register captured response streams\n";
        CefQuitMessageLoop();
        return;
      }
    }
    CefBrowserSettings settings;
    const char* initial_url="about:blank";
#ifdef FF_VIEWS_BENCHMARK
    const bool probe = CefCommandLine::GetGlobalCommandLine()->HasSwitch("probe-profile");
    if(probe) initial_url="http://127.0.0.1:18990/grit-fixed.html";
#endif
    auto view=CefBrowserView::CreateBrowserView(new Client,
      initial_url,settings,nullptr,
      CefRequestContext::GetGlobalContext(),new BrowserDelegate);
    auto window=CefWindow::CreateTopLevelWindow(new WindowDelegate(view));
    CefPostDelayedTask(TID_UI,new ShutdownTask(window,CefCommandLine::GetGlobalCommandLine()->HasSwitch("ff-direct-cage")),50);
  }
 private: IMPLEMENT_REFCOUNTING(App);
};
}
extern "C" NO_STACK_PROTECTOR int ff_browser(int argc,char** argv){
  umask(0077);CefMainArgs args(argc,argv);CefRefPtr<App> app=new App;
  int child=CefExecuteProcess(args,app,nullptr);if(child>=0)return child;
  auto command=CefCommandLine::CreateCommandLine();command->InitFromArgv(argc,argv);
  if(command->HasSwitch("ff-direct-cage"))fcntl(6,F_SETFD,FD_CLOEXEC);
  auto profile=command->GetSwitchValue("user-data-dir").ToString();
#ifdef FF_VIEWS_BENCHMARK
  if(profile.empty())profile=command->GetSwitchValue("probe-profile").ToString();
#endif
  if(profile.empty()){std::cerr<<"A private browser profile is required\n";return 2;}
  CefSettings settings;CefString(&settings.root_cache_path)=std::filesystem::absolute(profile).string();
  if(command->HasSwitch("disk-cache-dir")) CefString(&settings.cache_path)=(std::filesystem::absolute(profile)/"Default").string();
  if(!CefInitialize(args,settings,app,nullptr))return 1;
  std::signal(SIGTERM,stop);std::signal(SIGINT,stop);
  CefRunMessageLoop();CefShutdown();return 0;
}
