#pragma once
#include "include/cef_urlrequest.h"
#include <chrono>

namespace ff {
// Transport metadata selects a backend; it never changes the authored item or
// attests to rendering. All state/callbacks live on the creating CEF UI thread.
class SourceProbe final : public CefURLRequestClient {
 public:
  static CefRefPtr<SourceProbe> Start(const std::string& url);
  bool finished();
  void cancel();
  const std::string& mime() const { return mime_; }
  CefRefPtr<CefRequestContext> context() const { return context_; }
  void OnRequestComplete(CefRefPtr<CefURLRequest>) override;
  void OnUploadProgress(CefRefPtr<CefURLRequest>,int64_t,int64_t) override {}
  void OnDownloadProgress(CefRefPtr<CefURLRequest>,int64_t,int64_t) override {}
  void OnDownloadData(CefRefPtr<CefURLRequest>,const void*,size_t) override;
  bool GetAuthCredentials(bool,const CefString&,int,const CefString&,const CefString&,CefRefPtr<CefAuthCallback>) override { return false; }
 private:
  explicit SourceProbe(const std::string& url);
  void begin(bool head);
  bool inspect(CefRefPtr<CefURLRequest>);
  std::string url_,mime_;
  bool done_=false,head_=true;
  std::chrono::steady_clock::time_point deadline_;
  CefRefPtr<CefRequestContext> context_;
  CefRefPtr<CefURLRequest> request_;
  IMPLEMENT_REFCOUNTING(SourceProbe);
};
}
