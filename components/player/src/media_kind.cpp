#include "ff/media_kind.hpp"
#include <algorithm>
namespace ff {
std::string local_artwork_name(const std::string& source) {
  auto path = source.starts_with("file:///") ? source.substr(7) : source;
  if (!path.starts_with(local_artworks)) return {};
  return path.substr(sizeof(local_artworks) - 1);
}

bool virtual_cache_source(const std::string& source) {
  auto scheme=source.find("://");if(scheme==std::string::npos)return false;
  auto start=scheme+3;auto end=source.find_first_of("/:?#",start);
  auto host=source.substr(start,end-start);
  std::transform(host.begin(),host.end(),host.begin(),[](unsigned char c){return std::tolower(c);});
  return host.ends_with(".onchfs.invalid");
}

bool native_raster(const Json& item) {
  auto mime=item.value("mimeType","");
  std::transform(mime.begin(),mime.end(),mime.begin(),[](unsigned char c){return std::tolower(c);});
  auto source=item.value("source","");
  if(source.starts_with("data:image/")){auto declaration=source.substr(5,source.find_first_of(";,")-5);return !declaration.starts_with("image/svg");}
  auto path=source.substr(0,source.find_first_of("?#"));
  std::transform(path.begin(),path.end(),path.begin(),[](unsigned char c){return std::tolower(c);});
  // SVG remains an original browser document, including external resources,
  // scripts and its own CSP. Never hand vector XML to a raster decoder.
  if(mime.starts_with("image/svg")||path.ends_with(".svg")||path.ends_with(".svgz"))return false;
  if(mime.starts_with("image/"))return true;
  for(auto suffix:{".png",".apng",".jpg",".jpeg",".gif",".webp",".avif",".heif",".heic",".bmp",".tif",".tiff",".ico"})if(path.ends_with(suffix))return true;
  return false;
}
bool native_media(const Json& item) {
  if(native_raster(item))return true;
  if(item.value("source","").starts_with("data:video/")||item.value("source","").starts_with("data:audio/"))return true;
  auto mime=item.value("mimeType","");
  std::transform(mime.begin(),mime.end(),mime.begin(),[](unsigned char c){return std::tolower(c);});
  if(mime.starts_with("video/")||mime.starts_with("audio/")||mime=="application/vnd.apple.mpegurl"||mime=="application/x-mpegurl"||mime=="application/dash+xml")return true;
  auto source=item.value("source","");auto path=source.substr(0,source.find_first_of("?#"));
  std::transform(path.begin(),path.end(),path.begin(),[](unsigned char c){return std::tolower(c);});
  for(auto suffix:{".mp4",".m4v",".webm",".mov",".mkv",".avi",".mp3",".m4a",".aac",".oga",".ogg",".opus",".wav",".flac",".m3u8",".mpd",".ts",".mts",".m2ts",".hevc",".h265"})if(path.ends_with(suffix))return true;
  return false;
}
}
