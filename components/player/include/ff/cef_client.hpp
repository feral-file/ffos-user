#pragma once
#include "ff/presentation.hpp"
#include "include/cef_client.h"
#include "include/cef_app.h"
#include "include/cef_request_context.h"
#include "ff/input.hpp"

namespace ff {
class ArtworkClient : public CefClient, public CefLifeSpanHandler, public CefLoadHandler,
                      public CefRenderHandler, public CefRequestHandler, public CefPermissionHandler,
                      public CefDownloadHandler, public CefContextMenuHandler, public CefDisplayHandler {
 public:
  ArtworkClient(Playback& playback, Presentation& presentation, Occurrence occurrence, bool bundled);
  CefRefPtr<CefLifeSpanHandler> GetLifeSpanHandler() override { return this; }
  CefRefPtr<CefLoadHandler> GetLoadHandler() override { return this; }
  CefRefPtr<CefRenderHandler> GetRenderHandler() override { return this; }
  CefRefPtr<CefRequestHandler> GetRequestHandler() override { return this; }
  CefRefPtr<CefPermissionHandler> GetPermissionHandler() override { return this; }
  CefRefPtr<CefDownloadHandler> GetDownloadHandler() override { return this; }
  CefRefPtr<CefContextMenuHandler> GetContextMenuHandler() override { return this; }
  CefRefPtr<CefDisplayHandler> GetDisplayHandler() override { return this; }
  bool OnConsoleMessage(CefRefPtr<CefBrowser>,cef_log_severity_t,const CefString&,const CefString&,int) override;
  void OnAfterCreated(CefRefPtr<CefBrowser> browser) override;
  void OnBeforeClose(CefRefPtr<CefBrowser> browser) override;
  bool OnBeforePopup(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame>, int, const CefString&, const CefString&, cef_window_open_disposition_t, bool, const CefPopupFeatures&, CefWindowInfo&, CefRefPtr<CefClient>&, CefBrowserSettings&, CefRefPtr<CefDictionaryValue>&, bool*) override { return true; }
  bool OnBeforeBrowse(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame>, CefRefPtr<CefRequest>, bool, bool) override;
  void OnLoadEnd(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame>, int status) override;
  void OnLoadError(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame>, ErrorCode, const CefString&, const CefString&) override;
  void GetViewRect(CefRefPtr<CefBrowser>, CefRect& rect) override;
  void OnPaint(CefRefPtr<CefBrowser>, PaintElementType, const RectList&, const void*, int, int) override;
  void OnAcceleratedPaint(CefRefPtr<CefBrowser>, PaintElementType, const RectList&, const CefAcceleratedPaintInfo&) override;
  void OnRenderProcessTerminated(CefRefPtr<CefBrowser>, TerminationStatus, int, const CefString&) override;
  bool OnRenderProcessUnresponsive(CefRefPtr<CefBrowser>, CefRefPtr<CefUnresponsiveProcessCallback>) override;
  bool OnRequestMediaAccessPermission(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame>, const CefString&, uint32_t, CefRefPtr<CefMediaAccessCallback> callback) override { callback->Cancel(); return true; }
  bool OnShowPermissionPrompt(CefRefPtr<CefBrowser>, uint64_t, const CefString&, uint32_t, CefRefPtr<CefPermissionPromptCallback> callback) override { callback->Continue(CEF_PERMISSION_RESULT_DENY); return true; }
  bool OnBeforeDownload(CefRefPtr<CefBrowser>, CefRefPtr<CefDownloadItem>, const CefString&, CefRefPtr<CefBeforeDownloadCallback>) override { return true; }
  void OnBeforeContextMenu(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame>, CefRefPtr<CefContextMenuParams>, CefRefPtr<CefMenuModel> model) override { model->Clear(); }
  bool OnProcessMessageReceived(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame>, CefProcessId, CefRefPtr<CefProcessMessage>) override;
  void input(const Json& event);
  void update_media();
  void close();
  CefRefPtr<CefBrowser> browser;
  Surface surface;
  const std::string id;
  bool closed = false;
  bool closing = false;
  bool ready = false;
  bool has_ready_frame() const { return ready&&surface.frames>ready_after_frame_; }
  const bool bundled;
 private:
  Playback& playback_;
  Presentation& presentation_;
  Occurrence occurrence_;
  std::string media_state_;
  int view_width_=0,view_height_=0;
  uint64_t ready_after_frame_=0;
  void mark_ready();
  void configure();
  IMPLEMENT_REFCOUNTING(ArtworkClient);
};
CefRefPtr<CefApp> renderer_app();
void register_renderer_resources(CefRefPtr<CefRequestContext> context,const std::string& resources);
std::string renderer_url(const Occurrence& occurrence);
}
