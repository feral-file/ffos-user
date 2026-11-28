#include "ff/cache_client.hpp"
#include "ff/media_kind.hpp"
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
  auto read=[&](std::string target,Json headers,bool file,bool policy,std::string method="GET",std::shared_ptr<const ff::CaptureSnapshot> capture={}) {
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
  fs::remove(root/"blobs"/digest(bytes));
  auto delayed = read(asset,empty,true,false,"GET",policy.capture);
  check(delayed.error.empty() && fs::exists(delayed.metadata.at("path").get<std::string>()));
  auto delayed_part = read(asset,{{"Range","bytes=1-4"}},false,false,"GET",policy.capture);
  check(delayed_part.body=="ampl");
  record["resources"][1]["freshUntil"]="2001-01-01T00:00:00Z";
  std::ofstream(record_file)<<Json{{"automaticCacheVersion",1},{"record",record}};
  check(!read(url,empty,false,true).metadata.contains("replayMode"));
  record["automatic"]=false;
  std::ofstream(root/"blobs"/digest(bytes))<<bytes;
  record["resources"].push_back({{"url",url+"/analytics"},{"method","POST"},{"status",204}});
  std::ofstream(record_file)<<record;
  check(read(url,empty,false,true).error.empty());
  check(!read(url+"/analytics",empty,false,false,"POST").error.empty());
  record["resources"][1].erase("browserPolicyVersion");
  std::ofstream(record_file)<<record;
  auto legacy = read(url,empty,false,true);
  check(legacy.error.empty() && !legacy.capture && !legacy.metadata.contains("streams"));
  check(legacy.replay_mode(true) == "liveDocument");
  try {
    legacy.replay_mode(false);
    throw std::runtime_error("Legacy browser capture allowed offline");
  } catch (const ff::Error& e) {
    check(std::string(e.what()).find("recapture") != std::string::npos);
  }
  check(!read(asset,empty,false,false).error.empty());
  const auto original_url = url;
  for (const auto& alternate : {original_url + ".png", std::string("https://site.onchfs.invalid/work")}) {
    url = alternate;
    auto named = record;
    named["item"]["source"] = named["resources"][0]["url"] = url;
    std::ofstream(root/"items"/(digest(url)+".json")) << named;
    auto reply = read(url,empty,false,true);
    if (ff::virtual_cache_source(url)) {
      check(!reply.error.empty());
    } else {
      check(ff::native_media(named.at("item")) && reply.replay_mode(true) == "liveDocument");
    }
  }
  url = original_url;
  record["capsule"] = {{"archiveSHA256", std::string(64,'c')}};
  std::ofstream(record_file)<<record;
  check(!read(url,empty,false,true).error.empty());
  record.erase("capsule");
  record["resources"][1]["browserPolicyVersion"] = 3;
  std::ofstream(record_file)<<record;
  check(!read(url,empty,false,true).error.empty());
  record["resources"][1]["browserPolicyVersion"] = 2;
  std::ofstream(record_file)<<record;
  auto refreshed = read(url,empty,false,true);
  check(refreshed.capture && refreshed.replay_mode(false) == "isolated" &&
        refreshed.replay_mode(true) == "isolated");
  const std::string archive(64, 'b');
  const std::string mount = "https://" + archive.substr(0,32) + "." + archive.substr(32) + ".capsule.invalid/";
  // A signed replay with an extensionless source uses the same MIME handoff
  // to libmpv as an ordinary captured video.
  auto video = clip;
  video["url"] = url;
  auto replay = Json{{"item",{{"source",url}}},{"entry",url},
                     {"capsule",{{"archiveSHA256",archive}}},
                     {"resources",Json::array({video})},{"coverage",{{"complete",true}}}};
  std::ofstream(record_file) << replay;
  auto video_policy = read(url,empty,false,true);
  check(video_policy.capture && video_policy.metadata.value("replayMode","") == "isolated");
  auto handoff = read(url,empty,false,false,"GET",video_policy.capture);
  check(handoff.metadata.value("nativeMime","") == "video/mp4");
  auto media_file = read(url,empty,true,false,"GET",video_policy.capture);
  check(media_file.error.empty() && fs::exists(media_file.metadata.at("path").get<std::string>()));
  replay["item"]["mimeType"] = "video/mp4";
  replay["resources"][0]["contentType"] = "Application/Octet-Stream; charset=binary";
  std::ofstream(record_file) << replay;
  auto generic = read(url,empty,false,true);
  check(generic.capture && ff::native_media(generic.capture->render_item(replay.at("item"))));
  check(!ff::native_raster(generic.capture->render_item(Json{{"source","https://example.org/work.png"}})));
  replay["resources"] = Json::array({document, clip});
  replay["resources"][1]["contentType"] = "image/svg+xml";
  std::ofstream(record_file) << replay;
  auto vector_policy = read(url,empty,false,true);
  check(vector_policy.capture && vector_policy.capture->entry_mime == "image/svg+xml");
  check(!ff::native_media(vector_policy.capture->render_item(Json{{"source", "https://example.org/work.png"}})));
  auto named = clip;
  named["url"] = mount + "work.png";
  named["contentType"] = "image/png";
  auto files = Json{{"item",{{"source",url}}},{"entry",named.at("url")},
                    {"capsule",{{"files",true},{"archiveSHA256",archive}}},
                    {"resources",Json::array({named})},{"coverage",{{"complete",true}}}};
  for (auto [path, mime] : {std::pair{"work.png","image/png"}, {"clip.mp4","video/mp4"},
                             {"model.glb","model/gltf-binary"}}) {
    files["entry"] = files["resources"][0]["url"] = mount + path;
    files["resources"][0]["contentType"] = mime;
    std::ofstream(record_file) << files;
    auto mounted = read(url,empty,false,true);
    check(mounted.capture && mounted.capture->file_entry == mount + path && mounted.capture->entry_mime == mime);
    auto variant = read(mount + path + "?v=1",empty,true,false,"GET",mounted.capture);
    check(variant.error.empty() && fs::exists(variant.metadata.at("path").get<std::string>()));
    check(!read(url + "?v=1",empty,true,false,"GET",mounted.capture).error.empty());
  }
  files["entry"] = files["resources"][0]["url"] = mount + "sketch%281%29.js";
  std::ofstream(record_file) << files;
  auto mounted = read(url,empty,false,true);
  check(mounted.capture && read(mount + "sketch(1).js?v=1",empty,false,false,"GET",mounted.capture).body == bytes);
  files["entry"] = "https://example.org/foreign";
  std::ofstream(record_file) << files;
  check(!read(url,empty,false,true).error.empty());
  fs::remove_all(root);
}
