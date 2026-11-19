#pragma once
#include <atomic>
#include <deque>
#include <memory>
#include <mutex>

#include "ff/json.hpp"
namespace ff {
struct CacheReply {
  std::string session, request, error;
  bool policy = false;
  Json metadata;
  std::string body;
};
struct CacheInbox {
  std::atomic<bool> active{true};
  std::mutex mutex;
  std::deque<CacheReply> replies;
  size_t outstanding = 0; // queued + running + completed but not consumed
  std::deque<CacheReply> take() {
    std::lock_guard lock(mutex);
    std::deque<CacheReply> result;
    result.swap(replies);
    outstanding -= result.size();
    return result;
  }
};
bool cache_request(const std::shared_ptr<CacheInbox>& inbox, const std::string& socket,
                   const std::string& session, const std::string& request, const Json& params,
                   bool open_file = false, bool policy = false);
std::string base64(const std::string& bytes);
}  // namespace ff
