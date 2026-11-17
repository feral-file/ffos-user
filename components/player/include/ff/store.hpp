#pragma once
#include "ff/json.hpp"
struct sqlite3;
namespace ff {
// A single writer owns the database. A commit precedes any externally visible
// change of durable intent; a failed write leaves the live state untouched.
class Store {
 public:
  explicit Store(const std::string& path);
  ~Store();
  Store(const Store&) = delete;
  Store& operator=(const Store&) = delete;
  Json load();
  void save(const Json& state);
 private:
  sqlite3* db_ = nullptr;
  int lock_ = -1;
  void exec(const char* sql);
};
Json initial_state();
}
