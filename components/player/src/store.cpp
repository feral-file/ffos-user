#include "ff/store.hpp"
#include <sqlite3.h>
#include <memory>
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
