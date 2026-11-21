#include "ff/cache_client.hpp"
#include <glib.h>
#include <filesystem>
#include <fstream>
#include <thread>
#include <stdexcept>
#include <unistd.h>

using ff::Json;
namespace fs = std::filesystem;
void check(bool ok) { if (!ok) throw std::runtime_error("bundle reader assertion failed"); }
std::string digest(const std::string& bytes) {
  auto result = g_compute_checksum_for_string(G_CHECKSUM_SHA256, bytes.data(), bytes.size());
  std::string value(result);
  g_free(result);
  return value;
}
int main() {
  char tmp[]="/tmp/ff-bundle-test-XXXXXX";
  check(mkdtemp(tmp));
  fs::path root(tmp);
  fs::create_directories(root/"items");
  fs::create_directories(root/"blobs");
  std::string url="https://example.org/work", asset=url+"/clip.mp4", bytes="sample bytes";
  std::ofstream(root/"blobs"/digest(bytes)) << bytes;
  auto record_file=root/"items"/(digest(url)+".json");
  Json clip={{"url",asset},{"sha256",digest(bytes)},{"contentType","video/mp4"},
             {"freshUntil","2099-01-01T00:00:00Z"},{"browserPolicyVersion",2}};
  Json document={{"url",url},{"contentType","text/html"},{"browserPolicyVersion",2},
                 {"freshUntil","2099-01-01T00:00:00Z"},{"redirectTo","work/clip.mp4"}};
  Json record={{"item",{{"source",url}}},{"coverage",{{"complete",true}}},
               {"resources",Json::array({document,clip})},{"automatic",true}};
  auto read=[&](std::string target,Json headers,bool file,bool policy,std::string method="GET",std::shared_ptr<const Json> capture={}) {
    auto inbox=std::make_shared<ff::CacheInbox>();
    check(ff::cache_request(inbox,root,"session","request",{{"source",url},{"url",target},{"method",method},{"headers",headers}},file,policy,capture));
    for(int i=0;i<500;++i) {
      auto replies=inbox->take();
      if(!replies.empty()) return replies.front();
      std::this_thread::sleep_for(std::chrono::milliseconds(10));
    }
    throw std::runtime_error("bundle reader timed out");
  };
  auto empty=Json::object();
  std::ofstream(record_file)<<Json{{"automaticCacheVersion",1},{"record",record}};
  check(read(url,empty,false,true).metadata.value("replayMode","")=="mixed");
  check(read(url,empty,true,false).metadata.at("path")== (root/"blobs"/digest(bytes)).string());
  auto part=read(asset,{{"Range","bytes=1-4"}},false,false);
  check(part.error.empty() && part.metadata.at("status")==206 && part.body=="ampl");
  auto past=read(asset,{{"Range","bytes=12-"}},false,false);
  check(past.error.empty() && past.metadata.at("status")==416 && past.body.empty());
  check(read(asset,empty,false,false,"HEAD").body.empty());
  for (auto method : {"HEAD", "OPTIONS"}) {
    auto captured = clip;
    captured["method"] = method;
    captured["status"] = 204;
    record["resources"].push_back(captured);
    std::ofstream(record_file)<<record;
    auto response = read(asset,empty,false,false,method);
    check(response.error.empty() && response.metadata.at("status")==204 && response.body.empty());
  }
  auto large = clip;
  const std::string large_hash(64, 'a');
  const std::string large_url = url + "/large.bin";
  large["url"] = large_url;
  large["sha256"] = large_hash;
  large["contentType"] = "application/octet-stream";
  std::ofstream(root/"blobs"/large_hash).put('x');
  fs::resize_file(root/"blobs"/large_hash, 40 * 1024 * 1024);
  record["resources"].push_back(large);
  std::ofstream(record_file)<<record;
  auto stream = read(large_url,empty,false,false);
  check(stream.body.empty() && stream.metadata.at("streamHash")==large_hash);
  auto policy = read(url,empty,false,true);
  check(policy.metadata.at("streams").at(large_url).at("hash")==large_hash);
  record["resources"][1]["headerFields"] = Json::array({{{"name","Content-Security-Policy"},{"value","default-src 'none'"}}});
  std::ofstream(record_file)<<record;
  auto pinned = read(asset,empty,false,false,"GET",policy.capture);
  check(pinned.metadata.at("headers").empty());
  check(!read(asset,empty,false,false).metadata.at("headers").empty());
  record["resources"][1]["freshUntil"]="2001-01-01T00:00:00Z";
  std::ofstream(record_file)<<Json{{"automaticCacheVersion",1},{"record",record}};
  check(!read(url,empty,false,true).metadata.contains("replayMode"));
  record["automatic"]=false;
  record["resources"][1].erase("browserPolicyVersion");
  std::ofstream(record_file)<<record;
  check(!read(url,empty,false,true).error.empty());
  fs::remove_all(root);
}
