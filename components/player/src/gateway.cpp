#include "ff/artwork_cache.hpp"
#include <glib.h>
#include <cstdlib>

extern "C" char* ff_gateway_url(const char*, const char*);
namespace ff {
ArtworkRoute artwork_route(const Json& sources, const std::string& url) {
  ArtworkRoute result{url, ""};
  if (!sources.contains("gatewayHosts")) return result;
  GUri* uri = g_uri_parse(url.c_str(), G_URI_FLAGS_ENCODED, nullptr);
  if (!uri) return result;
  const char* host = g_uri_get_host(uri);
  for (const auto& candidate : sources.at("gatewayHosts")) {
    if (!host || g_ascii_strcasecmp(host, candidate.get_ref<const std::string&>().c_str())) continue;
    char* replacement = ff_gateway_url(url.c_str(), sources.value("gateway", "").c_str());
    if (replacement) {
      result.url = replacement;
      std::free(replacement);
    } else result.user_agent = sources.value("userAgent", "");
    break;
  }
  g_uri_unref(uri);
  return result;
}
}
