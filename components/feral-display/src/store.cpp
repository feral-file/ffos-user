#include "ff/store.hpp"
#include <sqlite3.h>
#include <memory>
#include <cstdio>
#include <cerrno>
#include <iostream>
#include <filesystem>
#include <fcntl.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <unistd.h>

namespace ff {
using Statement = std::unique_ptr<sqlite3_stmt, decltype(&sqlite3_finalize)>;
Json initial_state() {
  return {{"version", 1}, {"playlist", nullptr}, {"index", 0}, {"context", "curated"},
          {"stopped", true}, {"sleeping", false}, {"paused", false}, {"loop", "playlist"},
          {"shuffle", false}, {"order", Json::array()},
          {"settings", {{"scaling", "fit"}, {"framing", "artwork"}, {"tombstone", "timed"}, {"transitionMs", 650},
            {"durationSeconds", nullptr}, {"margin", 0}, {"background", "#000000"}, {"rotation", 0}, {"volume", 1.0}, {"muted", false}}},
          {"policy", {{"version", 1}, {"showMatureContent", false}, {"strictPersonal", false}, {"blockUnratedCurated", false}}},
          {"history", Json::array()}, {"historyIncomplete", false}, {"boot", nullptr}, {"schedule", nullptr}};
}
Store::Store(const std::string& path) {
  auto canonical=std::filesystem::weakly_canonical(path).string();
  policy_path_ = std::filesystem::path(canonical).parent_path() / "global-policy.json";
  lock_=open((canonical+".lock").c_str(),O_CREAT|O_RDWR|O_CLOEXEC|O_NOFOLLOW,0600);
  require(lock_>=0,"storage_unavailable","Cannot open persistent-state ownership lock");
  struct stat lock_stat{};
  if(fstat(lock_,&lock_stat)!=0||!S_ISREG(lock_stat.st_mode)||lock_stat.st_uid!=geteuid()){
    close(lock_);lock_=-1;throw Error("storage_unavailable","Persistent-state lock must be a regular file owned by this user");
  }
  if(flock(lock_,LOCK_EX|LOCK_NB)!=0){close(lock_);lock_=-1;throw Error("storage_in_use","Another native runtime owns this state directory");}
  int rc = sqlite3_open_v2(canonical.c_str(), &db_, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_FULLMUTEX, nullptr);
  if (rc != SQLITE_OK) {
    std::string message = db_ ? sqlite3_errmsg(db_) : "Cannot open state database";
    if (db_) sqlite3_close(db_);
    db_ = nullptr;
    close(lock_);lock_=-1;
    throw Error("storage_unavailable", message);
  }
  try {
    sqlite3_busy_timeout(db_, 1000);
    exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;");
    exec("CREATE TABLE IF NOT EXISTS state (id INTEGER PRIMARY KEY CHECK(id=1), document TEXT NOT NULL);");
  } catch (...) { sqlite3_close(db_); db_ = nullptr; close(lock_);lock_=-1;throw; }
}
Store::~Store() { if (db_) sqlite3_close(db_); if(lock_>=0)close(lock_); }
void Store::exec(const char* sql) {
  if (sqlite3_exec(db_, sql, nullptr, nullptr, nullptr) != SQLITE_OK)
    throw Error("storage_unavailable", sqlite3_errmsg(db_));
}
Json Store::load() {
  sqlite3_stmt* raw = nullptr;
  require(sqlite3_prepare_v2(db_, "SELECT document FROM state WHERE id=1", -1, &raw, nullptr) == SQLITE_OK,
          "storage_unavailable", sqlite3_errmsg(db_));
  Statement stmt(raw, sqlite3_finalize);
  int rc = sqlite3_step(raw);
  if (rc == SQLITE_DONE) return initial_state();
  require(rc == SQLITE_ROW, "storage_unavailable", sqlite3_errmsg(db_));
  auto result = parse(reinterpret_cast<const char*>(sqlite3_column_text(raw, 0)));
  require(result.is_object() && result.value("version", 0) == 1, "storage_version", "Unsupported persistent-state version; preserve database for rollback");
  return result;
}
namespace {
bool sync_parent_directory(const std::string& path) {
  auto directory = std::filesystem::path(path).parent_path();
  int parent = open(directory.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
  if (parent < 0) return false;
  bool synced = fsync(parent) == 0;
  close(parent);
  return synced;
}

Json default_global_policy() {
  return {{"autoplay", true}, {"localNetwork", false}, {"geolocation", false},
          {"camera", false}, {"microphone", false}, {"notifications", false},
          {"cache", {{"maxAgeDays", 30}, {"maxBytes", 1073741824}}}};
}
}

void validate_global_policy(const Json& policy) {
  fields(policy, {"autoplay","localNetwork","geolocation","camera","microphone","notifications","cache"});
  for (const auto& [name, value] : policy.items()) {
    if (name == "cache") {
      fields(value, {"maxAgeDays", "maxBytes"});
      for (const auto& [key, limit] : value.items()) {
        const int64_t maximum = key == "maxAgeDays" ? 36500 : (1LL << 50);
        require(limit.is_number_integer() && limit > 0 && limit <= maximum,
                "invalid_params", "Invalid cache limit: " + key);
      }
      continue;
    }
    require(value.is_boolean(), "invalid_params", "Expected boolean: " + name);
  }
}

Json Store::load_global_policy(Json& playback_state) {
  auto policy = default_global_policy();
  int fd = open(policy_path_.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK);
  if (fd < 0 && errno == ENOENT) {
    if (playback_state.contains("artworkPermissions")) {
      const auto& legacy = playback_state.at("artworkPermissions");
      validate_global_policy(legacy);
      policy.update(legacy, true);
    }
    require(save_global_policy(policy), "storage_unavailable", "Cannot sync global policy directory");
  } else {
    require(fd >= 0, "storage_unavailable", "Cannot open " + policy_path_);
    auto close_file = [](FILE* stream) { fclose(stream); };
    std::unique_ptr<FILE, decltype(close_file)> file(fdopen(fd, "r"), close_file);
    if (!file) close(fd);
    require(bool(file), "storage_unavailable", "Cannot read " + policy_path_);
    struct stat info{};
    require(fstat(fd, &info) == 0 && S_ISREG(info.st_mode), "storage_unavailable",
            "Expected a regular file at " + policy_path_);
    char buffer[16385];
    size_t size = fread(buffer, 1, sizeof(buffer), file.get());
    require(!ferror(file.get()), "storage_unavailable", "Read failed for " + policy_path_);
    try {
      require(size < sizeof(buffer), "invalid_params", "Global policy exceeds 16 KiB");
      auto saved = parse(std::string_view(buffer, size));
      validate_global_policy(saved);
      policy.update(saved, true);
    } catch (const Error& e) {
      // Preserve the operator's file and discard the entire invalid grant set.
      std::cerr << policy_path_ << ": " << e.what() << "; using default permissions\n";
    }
  }
  // Publish the file first. A restart between writes will prefer that file
  // and repeat only the SQLite cleanup.
  if (playback_state.erase("artworkPermissions")) {
    require(sync_parent_directory(policy_path_), "storage_unavailable", "Cannot sync migrated global policy");
    save(playback_state);
  }
  return policy;
}

bool Store::save_global_policy(const Json& policy) {
  const auto contents = policy.dump(2) + "\n";
  auto temporary = policy_path_ + ".XXXXXX";
  int fd = mkstemp(temporary.data());
  require(fd >= 0, "storage_unavailable", "Cannot create temporary global policy file");
  try {
    size_t offset = 0;
    while (offset < contents.size()) {
      auto n = write(fd, contents.data() + offset, contents.size() - offset);
      if (n < 0 && errno == EINTR) continue;
      require(n > 0, "storage_unavailable", "Cannot write global policy");
      offset += static_cast<size_t>(n);
    }
    require(fsync(fd) == 0, "storage_unavailable", "Cannot sync global policy");
    auto status = close(fd);
    fd = -1;
    require(status == 0 && rename(temporary.c_str(), policy_path_.c_str()) == 0,
            "storage_unavailable", "Cannot publish global policy");
  } catch (...) {
    if (fd >= 0) close(fd);
    unlink(temporary.c_str());
    throw;
  }
  return sync_parent_directory(policy_path_);
}

void Store::save(const Json& state) {
  std::string text = state.dump();
  require(text.size() <= max_frame, "storage_limit", "Persistent state exceeds 4 MiB");
  exec("BEGIN IMMEDIATE");
  try {
    sqlite3_stmt* raw = nullptr;
    require(sqlite3_prepare_v2(db_, "INSERT INTO state(id,document) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET document=excluded.document", -1, &raw, nullptr) == SQLITE_OK,
            "storage_unavailable", sqlite3_errmsg(db_));
    Statement stmt(raw, sqlite3_finalize);
    require(sqlite3_bind_text(raw, 1, text.c_str(), static_cast<int>(text.size()), SQLITE_TRANSIENT) == SQLITE_OK,
            "storage_unavailable", sqlite3_errmsg(db_));
    require(sqlite3_step(raw) == SQLITE_DONE, "storage_unavailable", sqlite3_errmsg(db_));
    exec("COMMIT");
  } catch (...) { sqlite3_exec(db_, "ROLLBACK", nullptr, nullptr, nullptr); throw; }
}
}
