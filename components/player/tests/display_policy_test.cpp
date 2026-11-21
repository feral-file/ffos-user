#include "ff/display.hpp"
#include <iostream>
#include <stdexcept>

namespace {
void check(bool passed, const char* message) {
  if (!passed) throw std::runtime_error(message);
}

void check_display_policy() {
  using ff::Json;
  const Json settings = {{"margin", 1}, {"framing", "fill"}};
  Json defaults = {{"display", {{"margin", 2}, {"userOverrides", {{"scaling", false}}}}}};
  Json item = Json::object();
  auto margin = [&] { return ff::resolve_display(settings, defaults, item).at("margin"); };
  check(margin() == 2, "Playlist defaults override device margin");
  item["inlineManifest"]["controls"]["display"] = {{"margin", 3}};
  check(margin() == 3, "Inline manifest overrides playlist defaults");
  item["resolvedManifest"]["controls"]["display"] = {{"margin", 4}};
  check(margin() == 4, "Resolved manifest overrides inline manifest");
  item["override"]["display"] = {{"margin", 5}};
  check(margin() == 5, "Item override takes precedence over manifests");
  item["display"] = {{"margin", 6}, {"userOverrides", {{"margin", false}}}};
  const Json adjustment = {{"margin", 7}, {"scaling", "stretch"}};
  auto locked = ff::resolve_display(settings, defaults, item, adjustment);
  check(locked.at("margin") == 6 && locked.at("scaling") == "fit",
        "Field locks accumulate and apply to preview and device framing");
  item["display"]["overridePermissions"] = {{"margin", true}};
  check(ff::resolve_display(settings, defaults, item, adjustment) == locked,
        "Authored input cannot set the derived permission map");

  item["display"]["userOverrides"] = true;
  auto unlocked = ff::resolve_display(settings, defaults, item, adjustment);
  check(!unlocked.contains("overridePermissions") && unlocked.at("margin") == 7 &&
        unlocked.at("scaling") == "fill", "A boolean resets inherited locks and device framing applies last");
}
}  // namespace

int main() {
  try {
    check_display_policy();
  } catch (const std::exception& error) {
    std::cerr << error.what() << '\n';
    return 1;
  }
  return 0;
}
