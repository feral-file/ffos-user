#pragma once
#include "ff/json.hpp"
namespace ff {
inline constexpr char local_artworks[] = "/home/feralfile/artworks/";
std::string local_artwork_name(const std::string& source);
 bool virtual_cache_source(const std::string& source);
bool native_raster(const Json& item); bool native_media(const Json& item); }
