#include "ff/profile_gc.hpp"

#include <condition_variable>
#include <deque>
#include <filesystem>
#include <iostream>
#include <mutex>
#include <thread>

#include "ff/json.hpp"
namespace ff {
namespace {
bool occurrence_name(const std::string& name) {
  if (name.size() != 36) return false;
  for (size_t i = 0; i < name.size(); ++i) {
    if (i == 8 || i == 13 || i == 18 || i == 23) {
      if (name[i] != '-') return false;
    } else if (!std::isxdigit(static_cast<unsigned char>(name[i])))
      return false;
  }
  return true;
}
struct Janitor {
  struct Job {
    std::string path;
    bool sweep;
  };
  std::mutex mutex;
  std::condition_variable ready;
  std::deque<Job> jobs;
  Janitor() {
    std::thread([this] {
      while (true) {
        Job job;
        {
          std::unique_lock lock(mutex);
          ready.wait(lock, [&] { return !jobs.empty(); });
          job = std::move(jobs.front());
          jobs.pop_front();
        }
        std::error_code error;
        if (job.sweep) {
          auto root = std::filesystem::path(job.path);
          auto prefix = root.filename().string() + ".retired-";
          for (std::filesystem::directory_iterator it(root.parent_path(), error), end;
               !error && it != end; it.increment(error)) {
            auto name = it->path().filename().string();
            if (name.starts_with(prefix) && occurrence_name(name.substr(prefix.size()))) {
              std::error_code ignored;
              std::filesystem::remove_all(it->path(), ignored);
            }
          }
        } else if (occurrence_name(std::filesystem::path(job.path).filename().string()))
          std::filesystem::remove_all(job.path, error);
        if (error && error != std::errc::no_such_file_or_directory)
          std::cerr << "Browser profile cleanup: " << error.message() << '\n';
      }
    }).detach();
  }
  void push(Job job) {
    std::lock_guard lock(mutex);
    if (jobs.size() < 64) {
      jobs.push_back(std::move(job));
      ready.notify_one();
    } else
      std::cerr << "Browser profile cleanup deferred until next startup\n";
  }
};
// The process owns this I/O worker. Shutdown never joins an unbounded recursive
// filesystem deletion; systemd terminates it with the rest of the host.
Janitor& janitor() {
  static auto* value = new Janitor;
  return *value;
}
}  // namespace
void sweep_browser_profiles(const std::string& directory) {
  // Move the previous epoch aside before starting I/O. New occurrence paths
  // cannot race the startup sweep, even when playback starts immediately.
  auto root = std::filesystem::path(directory);
  std::error_code error;
  if (!std::filesystem::exists(root, error)) return;
  auto retired = root;
  retired += ".retired-" + uuid();
  std::filesystem::rename(root, retired, error);
  if (error) std::cerr << "Browser profile retirement: " << error.message() << '\n';
  janitor().push({root.string(), true});
}
void retire_browser_profile(const std::string& directory) { janitor().push({directory, false}); }
}  // namespace ff
