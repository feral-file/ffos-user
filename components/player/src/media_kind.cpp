#include "ff/media_kind.hpp"
#include <algorithm>
namespace ff {
bool native_media(const Json& item) {
  auto mime=item.value("mimeType","");
  std::transform(mime.begin(),mime.end(),mime.begin(),[](unsigned char c){return std::tolower(c);});
  if(mime.starts_with("video/")||mime.starts_with("audio/")||mime=="application/vnd.apple.mpegurl"||mime=="application/x-mpegurl"||mime=="application/dash+xml")return true;
  auto source=item.value("source","");auto path=source.substr(0,source.find_first_of("?#"));
  std::transform(path.begin(),path.end(),path.begin(),[](unsigned char c){return std::tolower(c);});
  for(auto suffix:{".mp4",".m4v",".webm",".mov",".mkv",".avi",".mp3",".m4a",".aac",".oga",".ogg",".opus",".wav",".flac",".m3u8",".mpd"})if(path.ends_with(suffix))return true;
  return false;
}
}
