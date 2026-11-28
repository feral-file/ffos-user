#include "ff/artwork_cache.hpp"
#include <glib.h>
#include <cstdlib>

extern "C" char* ff_gateway_document_url(const char*);
extern "C" char* ff_gateway_url(const char*, const char*);
extern "C" char* ff_observed_gateway_url(const char*, const char*, const char*);
namespace ff {
ArtworkRoute artwork_route(const Json& sources, const Json& item, const std::string& url, bool browser) {
  ArtworkRoute result{url, ""};

  GUri* uri = g_uri_parse(url.c_str(), G_URI_FLAGS_ENCODED, nullptr);
  if (!uri) return result;
  const char* host = g_uri_get_host(uri);
  for (const auto& candidate : sources.value("gatewayHosts", Json::array())) {
    if (!host || g_ascii_strcasecmp(host, candidate.get_ref<const std::string&>().c_str())) continue;
    char* replacement = ff_gateway_url(url.c_str(), sources.value("gateway", "").c_str());
    if (replacement) {
      result.url = replacement;
      std::free(replacement);
    } else result.user_agent = sources.value("userAgent", "");
    break;
  }
  g_uri_unref(uri);
  if (item.contains("ffObservedGateway") && item.at("ffObservedGateway").is_string()) {
    char* observed = ff_observed_gateway_url(item.at("source").get_ref<const std::string&>().c_str(),
        item.at("ffObservedGateway").get_ref<const std::string&>().c_str(), url.c_str());
    if (observed) {
      // A byte probe accepts Filebase HTML even when its CSP prevents the
      // edition from starting. Other observed routes still rescue unavailable CIDs.
      const bool blocks_bootstrap = browser && result.url != url &&
          std::string_view(observed).starts_with("https://ipfs.filebase.io/");
      if (!blocks_bootstrap) {
        result.url = observed;
        result.user_agent.clear();
      }
      std::free(observed);
      return result;
    }
  }
  return result;
}
// A transparent request rewrite does not change document.baseURI. Carry
// the slash added by the configured CID gateway into the document URL too,
// so relative scripts stay under the CID and the edition query survives.
std::string gateway_document_url(const std::string& source, const std::string& routed) {
  if (source == routed) return source;
  auto normalized = ff_gateway_document_url(source.c_str());
  if (!normalized) return source;
  std::string url(normalized);
  std::free(normalized);
  return url;
}
}
