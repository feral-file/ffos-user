// Original-origin GET responses that exceed the engine control pipe's body limit.
#pragma once
#include "include/cef_scheme.h"
#include "include/wrapper/cef_stream_resource_handler.h"
#include <glib.h>
#include <json.hpp>
#include <ctime>
#include <fstream>
#include <memory>

namespace {
class CapturedStream final : public CefStreamResourceHandler {
 public:
  CapturedStream(int status, std::string mime, CefResponse::HeaderMap headers,
                 CefRefPtr<CefStreamReader> stream, std::string hash)
      : CefStreamResourceHandler(status, "", mime, std::move(headers), stream),
        stream_(stream), hash_(std::move(hash)) {}

  bool Open(CefRefPtr<CefRequest> request, bool& handle, CefRefPtr<CefCallback> callback) override {
    // CEF calls Open on a blocking thread. Hash the same open object that will
    // supply the response; a failed check cancels it before emitting any bytes.
    std::unique_ptr<GChecksum, decltype(&g_checksum_free)> digest(g_checksum_new(G_CHECKSUM_SHA256), g_checksum_free);
    unsigned char block[65536];
    while (auto size = stream_->Read(block, 1, sizeof(block)))
      g_checksum_update(digest.get(), block, size);
    if (hash_ != g_checksum_get_string(digest.get()) || stream_->Seek(0, SEEK_SET) != 0) {
      handle = true;
      return false;
    }
    return CefStreamResourceHandler::Open(request, handle, callback);
  }

 private:
  CefRefPtr<CefStreamReader> stream_;
  std::string hash_;
};

class CaptureFactory final : public CefSchemeHandlerFactory {
 public:
  explicit CaptureFactory(nlohmann::json streams) : streams_(std::move(streams)) {}

  CefRefPtr<CefResourceHandler> Create(CefRefPtr<CefBrowser>, CefRefPtr<CefFrame>, const CefString&,
                                        CefRefPtr<CefRequest> request) override {
    if (request->GetMethod() != "GET") return nullptr;
    auto found = streams_.find(request->GetURL().ToString());
    if (found == streams_.end()) return nullptr;
    auto until = found->at("until").get<int64_t>();
    if (until && until <= std::time(nullptr)) return nullptr;
    CefRequest::HeaderMap requested;
    request->GetHeaderMap(requested);
    for (const auto& [name, value] : requested) {
      auto key = name.ToString();
      if (!value.empty() && (!g_ascii_strcasecmp(key.c_str(), "Range") ||
          !g_ascii_strcasecmp(key.c_str(), "Authorization") || !g_ascii_strcasecmp(key.c_str(),"Cookie"))) return nullptr;
    }
    auto hash = found->at("hash").get<std::string>();
    auto file = CefStreamReader::CreateForFile("/streams/" + hash);
    if (!file) return nullptr;
    CefResponse::HeaderMap headers;
    for (const auto& field : found->at("headers")) {
      auto name = field.at("name").get<std::string>();
      if (!g_ascii_strcasecmp(name.c_str(), "Content-Range") || !g_ascii_strcasecmp(name.c_str(), "Content-Length")) continue;
      headers.emplace(name, field.at("value").get<std::string>());
    }
    headers.emplace("Cache-Control", "no-store");
    auto mime = found->value("mime", "");
    int status = found->at("status");
    return new CapturedStream(status == 206 ? 200 : status, mime.empty() ? "application/octet-stream" : mime,
                              std::move(headers), file, std::move(hash));
  }
 private:
  const nlohmann::json streams_;
  IMPLEMENT_REFCOUNTING(CaptureFactory);
};
}
