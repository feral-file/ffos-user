#include "ff/overlay.hpp"
#include <algorithm>
#include <cmath>
#include <set>

namespace ff {
namespace {
std::string trim(std::string value) {
  auto first=value.find_first_not_of(" \t\r\n");
  if(first==std::string::npos)return "";
  return value.substr(first,value.find_last_not_of(" \t\r\n")-first+1);
}
bool http_url(const std::string& url) {
  auto start=url.starts_with("https://")?8u:url.starts_with("http://")?7u:0u;
  if(!start || url.size()<=start || url.find_first_of(" \t\r\n")!=std::string::npos)return false;
  auto end=url.find_first_of("/?#",start);return (end==std::string::npos?url.size():end)>start;
}
std::string wifi_escape(const std::string& value) {
  std::string out;for(char c:value){if(c=='\\'||c==';'||c==','||c==':'||c=='"')out+='\\';out+=c;}return out;
}
}
void validate_overlay(const Json& p) {
  fields(p,{"owner","state","fields"});
  auto owner=string_field(p,"owner",32),state=string_field(p,"state",64);
  require(owner=="setup"||owner=="pairing"||owner=="recovery","invalid_params","Invalid overlay owner");
  auto f=p.value("fields",Json::object());
  require(f.is_object()&&p.dump().size()<=16384,"invalid_params","Invalid overlay payload");
  for(auto key:{"title","reason","ssid","password","device_name","pairingCode","browserName","url","portal_url"})
    if(f.contains(key)) require(f[key].is_string()&&f[key].get_ref<const std::string&>().size()<=2048&&f[key].get_ref<const std::string&>().find('\0')==std::string::npos,"invalid_params","Invalid overlay text");
  if(owner!="setup")return; // Pairing/recovery schemas are migrated separately.
  static const std::set<std::string> states{"softap_qr","joining","join_failed","updating","claim_qr","ready","hidden","scanning","finalizing","factory_reset","connecting","setup_error"};
  require(states.contains(state),"unsupported","Unsupported setup overlay state");
  fields(f,{"reason","ssid","password","portal_url","client_attached","url","device_name","progress"});
  // Wi-Fi escaping can double byte length; cap credentials well below QR
  // byte-mode capacity instead of accepting a panel with an unencodable code.
  if(f.contains("password"))require(f["password"].get_ref<const std::string&>().size()<=128,"invalid_params","SoftAP password exceeds 128 bytes");
  if(f.contains("client_attached"))require(f["client_attached"].is_boolean(),"invalid_params","client_attached must be boolean");
  if(f.contains("progress"))require(f["progress"].is_number()&&std::isfinite(f["progress"].get<double>()),"invalid_params","progress must be finite");
  if(state=="softap_qr")require(f.contains("ssid")&&!f["ssid"].get_ref<const std::string&>().empty()&&f["ssid"].get_ref<const std::string&>().size()<=128,"invalid_params","SoftAP SSID is required");
  if(state=="claim_qr")require(f.contains("url")&&http_url(f["url"].get<std::string>()),"invalid_params","Claim QR requires an HTTP(S) URL");
}
namespace {
// Only these escaped values may enter the trusted Pango template markup.
std::string markup_text(const std::string& value) {
  std::string result;
  for (char c : value) {
    if (c == '&') result += "&amp;";
    else if (c == '<') result += "&lt;";
    else if (c == '>') result += "&gt;";
    else result += c;
  }
  return result;
}
std::string bold(const std::string& value) { return "<b>" + markup_text(value) + "</b>"; }
}

OverlayPanel overlay_panel(const Json& p) {
  using enum PanelStyle;
  OverlayPanel panel;
  const auto f = p.value("fields", Json::object());
  const auto state = p.value("state", "");
  const auto owner = p.value("owner", "");
  const auto reason = markup_text(trim(f.value("reason", "")));
  auto& lines = panel.before_qr;

  if (owner == "pairing") {
    auto browser = trim(f.value("browserName", ""));
    if (browser.empty()) browser = "the browser";
    if (state == "pairing_code") {
      lines = {{title, "Pairing code"}, {code, markup_text(f.value("pairingCode", ""))},
               {subtitle, "Enter it on the website that wants to play art on this Art Computer."}};
    } else if (state == "creating_token") {
      lines = {{title, "Connecting " + markup_text(browser) + " to this Art Computer…"}};
    } else {
      if (browser.front() >= 'a' && browser.front() <= 'z') browser.front() -= 'a' - 'A';
      lines = {{title, markup_text(browser) + " wants to play art on this Art Computer."},
               {subtitle, "Open the Feral File app, go to Settings &gt; Art Computer, and approve the request."}};
    }
  } else if (owner != "setup") {
    lines = {{title, markup_text(f.value("title", state))}, {subtitle, reason}};
    panel.qr = f.value("url", "");
  } else if (state == "softap_qr") {
    const auto ssid = f.value("ssid", ""), password = f.value("password", "");
    const auto url = trim(f.value("portal_url", ""));
    if (f.value("client_attached", false) && http_url(url)) {
      lines = {{title, "Finish setup on your phone"}};
      panel.qr = url;
      panel.after_qr = {{compact, "Nothing opened? Scan the code, or mobile data/VPN off → " + bold(url) +
          "\nWi-Fi " + bold(ssid) + (password.empty() ? "" : " · Password " + bold(password)) + " · Keep connected"}};
    } else {
      lines = {{title, "Scan the QR code, then follow your phone's prompt to connect"}};
      if (!reason.empty()) lines.push_back({compact, reason});
      panel.qr = "WIFI:T:" + std::string(password.empty() ? "nopass" : "WPA") + ";S:" + wifi_escape(ssid) + ";";
      if (!password.empty()) panel.qr += "P:" + wifi_escape(password) + ";";
      panel.qr += ";";
      panel.after_qr = {{compact, "After you join, wait about 10 seconds: if this code changes, scan it again"
          "\nNothing opened? Wi-Fi Settings → " + bold(ssid) + "\n" +
          (password.empty() ? "" : "Password " + bold(password) + " · ") + "Keep connected" +
          (url.empty() ? "" : "\nStill stuck? Mobile data/VPN off → " + bold(url))}};
    }
  } else if (state == "claim_qr") {
    const auto name = trim(f.value("device_name", ""));
    panel.qr = f.value("url", "");
    lines = {{title, "Pair with the Feral File app"},
             {subtitle, "Connect your phone to the same Wi-Fi as this Art Computer."},
             {step, "Then open the app and look for " + (name.empty() ? "this Art Computer" : bold(name)) +
                    (panel.qr.empty() ? "." : ", or scan this code.")}};
    if (!panel.qr.empty())
      panel.after_qr = {{note, "Keep this code private: it can be used to pair with this Art Computer."}};
  } else if (state == "scanning") {
    lines = {{title, "Looking for Wi-Fi networks"}, {subtitle, "The setup screen will appear in a moment."}};
  } else if (state == "joining") {
    lines = {{title, "Connecting to Wi-Fi"}};
  } else if (state == "finalizing") {
    lines = {{title, "Wi-Fi connected"}, {subtitle, "Getting your Art Computer ready. This can take a minute."}};
  } else if (state == "connecting") {
    lines = {{title, "Connecting to the network"}};
    if (!reason.empty()) lines.push_back({subtitle, reason});
  } else if (state == "join_failed") {
    lines = {{title, "Couldn't connect to Wi-Fi"}, {subtitle, reason.empty() ? "Please try again." : reason}};
  } else if (state == "setup_error") {
    lines = {{title, "Setup needs attention"},
             {subtitle, reason.empty() ? "The Art Computer ran into a problem with setup mode. It will keep trying automatically." : reason}};
  } else if (state == "updating" || state == "factory_reset") {
    lines = {{title, state == "updating" ? "Updating software" : "Resetting to factory settings"}};
    if (state == "updating" && f.contains("progress")) {
      const int percent = std::round(std::clamp(f["progress"].get<double>(), 0.0, 100.0));
      lines.push_back({subtitle, std::to_string(percent) + "%"});
    }
    lines.push_back({subtitle, "Don't unplug it."});
  }
  return panel;
}
}
