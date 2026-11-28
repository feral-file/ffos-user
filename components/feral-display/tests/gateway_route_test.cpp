#include "ff/artwork_cache.hpp"
#include <stdexcept>

int main() {
  const std::string path = "/ipfs/QmePX7dqW9WFdm1rvNc5hmVmyJ2JdWbKxpCGzyEFPTCuRT/lib.js";
  const std::string source = "https://dweb.link" + path;
  const std::string configured = "https://ipfs.raribleuserdata.com";
  ff::Json sources = {{"gatewayHosts", {"dweb.link"}}, {"gateway", configured}};
  for (const std::string observed : {"https://ipfs.filebase.io", "https://gateway.pinata.cloud"}) {
    ff::Json item = {{"source", source}, {"ffObservedGateway", observed + path}};
    auto browser = ff::artwork_route(sources, item, source, true);
    const auto wanted = observed == "https://ipfs.filebase.io" ? configured : observed;
    if (browser.url != wanted + path) throw std::runtime_error("Unexpected browser gateway");
    if (ff::artwork_route(sources, item, source).url != observed + path)
      throw std::runtime_error("Native media lost its verified route");
  }
}
