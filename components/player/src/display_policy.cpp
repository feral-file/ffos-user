#include "ff/display.hpp"

namespace ff {
namespace {
void apply_display_layer(Json& display, const Json& layer) {
  for (const auto& [field, value] : layer.items()) {
    // overridePermissions is derived snapshot state. DP-1 authors express
    // both blanket rules and per-field exceptions through userOverrides.
    if (field == "overridePermissions") continue;
    // Earlier native builds persisted this spelling. Normalize it while
    // resolving the layer, leaving the signed playlist bytes untouched.
    if (field == "autoplay" && !value.is_boolean()) continue;
    if (field == "autoPlay") {
      if ((!layer.contains("autoplay") || !layer["autoplay"].is_boolean()) && value.is_boolean()) display["autoplay"] = value;
      continue;
    }
    if (field == "userOverrides") {
      if (value.is_object()) {
        display["overridePermissions"].update(value);
        continue;
      }
      display.erase("overridePermissions");
    }
    display[field] = value;
  }
}
}  // namespace

bool allows_display_override(const Json& display, const std::string& field) {
  const auto permissions = display.find("overridePermissions");
  if (permissions != display.end() && permissions->contains(field)) {
    return permissions->at(field).get<bool>();
  }
  return display.value("userOverrides", true);
}

Json resolve_display(const Json& settings, const Json& defaults, const Json& item,
                     const Json& adjustment) {
  Json display = {{"scaling", settings.value("scaling", "fit")},
                  {"margin", settings.value("margin", Json(0))},
                  {"background", settings.value("background", "#000000")},
                  {"autoplay", true}, {"loop", true}, {"userOverrides", true}};
  const Json empty = Json::object();
  apply_display_layer(display, defaults.value("display", empty));
  for (const auto* manifest : {"inlineManifest", "resolvedManifest"}) {
    apply_display_layer(display, item.value(manifest, empty).value("controls", empty).value("display", empty));
  }
  apply_display_layer(display, item.value("override", empty).value("display", empty));
  apply_display_layer(display, item.value("display", empty));
  for (const auto& [field, value] : adjustment.items()) {
    if (allows_display_override(display, field)) display[field] = value;
  }
  const auto framing = settings.value("framing", "artwork");
  if (framing != "artwork" && allows_display_override(display, "scaling")) display["scaling"] = framing;
  return display;
}
}  // namespace ff
