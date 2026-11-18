#include "ff/source_probe.hpp"
#include "include/cef_request_context_handler.h"
#include <algorithm>
#include <cctype>

namespace ff {
SourceProbe::SourceProbe(const std::string& url):url_(url),deadline_(std::chrono::steady_clock::now()+std::chrono::seconds(3)){
  CefRequestContextSettings settings;context_=CefRequestContext::CreateContext(settings,nullptr);
}
CefRefPtr<SourceProbe> SourceProbe::Start(const std::string& url){
  // Own the client before passing it to CEF, including Create's failure path.
  CefRefPtr<SourceProbe> probe=new SourceProbe(url);
  if(probe->context_)probe->begin(true);else probe->done_=true;
  return probe;
}
void SourceProbe::begin(bool head){
  head_=head;
  auto request=CefRequest::Create();request->SetURL(url_);request->SetMethod(head?"HEAD":"GET");
  // Some asset servers reject HEAD or omit its type. Range GET is a fallback,
  // canceled on the first body callback even if the server ignores Range.
  if(!head)request->SetHeaderByName("Range","bytes=0-511",true);
  request_=CefURLRequest::Create(request,this,context_);
  if(!request_)done_=true;
}
bool SourceProbe::inspect(CefRefPtr<CefURLRequest> request){
  auto response=request->GetResponse();
  if(!response||response->GetStatus()<200||response->GetStatus()>=300)return false;
  auto mime=response->GetMimeType().ToString();
  std::transform(mime.begin(),mime.end(),mime.begin(),[](unsigned char c){return std::tolower(c);});
  if(mime.empty()||mime=="application/octet-stream")return false;
  mime_=mime;return true;
}
void SourceProbe::OnRequestComplete(CefRefPtr<CefURLRequest> request){
  // CEF's API boundary can wrap the same request in a different C++ object.
  // Do not compare wrapper addresses. The only next request starts after this
  // one's terminal callback; cancel marks the probe done before releasing it.
  if(done_)return;
  bool known=request->GetRequestStatus()==UR_SUCCESS&&inspect(request);
  request_=nullptr;
  if(head_&&!known&&std::chrono::steady_clock::now()<deadline_){begin(false);return;}
  done_=true;
}
void SourceProbe::OnDownloadData(CefRefPtr<CefURLRequest> request,const void*,size_t){
  if(done_)return;
  inspect(request);cancel();
}
void SourceProbe::cancel(){
  done_=true;
  if(request_){auto request=request_;request_=nullptr;request->Cancel();}
}
bool SourceProbe::finished(){
  if(!done_&&std::chrono::steady_clock::now()>=deadline_)cancel();
  return done_;
}
}
