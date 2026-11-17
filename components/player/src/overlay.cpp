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
OverlayPanel overlay_panel(const Json& p) {
  const auto f=p.value("fields",Json::object()); auto state=p.value("state","");
  auto reason=trim(f.value("reason","")); OverlayPanel panel;
  if(p.value("owner","")!="setup") {
    panel.text=f.value("title",state)+"\n"+reason;
    for(auto key:{"pairingCode","browserName"})if(f.contains(key))panel.text+="\n"+f[key].get<std::string>();
    panel.qr=f.value("url","");return panel;
  }
  if(state=="softap_qr") {
    auto ssid=f.value("ssid",""),password=f.value("password",""),url=trim(f.value("portal_url",""));
    bool attached=f.value("client_attached",false)&&http_url(url);
    if(attached){panel.text="Finish setup on your phone\nNothing opened? Scan the code, or turn mobile data/VPN off and open "+url;panel.qr=url;}
    else {
      panel.text="Scan the QR code, then follow your phone's prompt to connect";
      if(!reason.empty())panel.text+="\n"+reason;
      panel.text+="\nAfter you join, wait about 10 seconds. If this code changes, scan it again.\nNothing opened? Open Wi-Fi Settings.";
      panel.qr="WIFI:T:"+std::string(password.empty()?"nopass":"WPA")+";S:"+wifi_escape(ssid)+";";
      if(!password.empty())panel.qr+="P:"+wifi_escape(password)+";";
      panel.qr+=";";
    }
    panel.text+="\nWi-Fi: "+ssid;
    if(!password.empty())panel.text+="  Password: "+password;
    panel.text+="\nKeep connected.";
    if(!attached&&!url.empty())panel.text+=" Still stuck? Turn mobile data/VPN off and open "+url;
  } else if(state=="claim_qr") {
    auto name=trim(f.value("device_name",""));if(name.empty())name="this Art Computer";
    panel.text="Pair with the Feral File app\nOpen the app on a phone on the same Wi-Fi and look for "+name+".\nOr scan this code.";panel.qr=f.value("url","");
  } else if(state=="scanning")panel.text="Looking for Wi-Fi networks\nThe setup screen will appear in a moment.";
  else if(state=="joining")panel.text="Connecting to Wi-Fi";
  else if(state=="finalizing")panel.text="Wi-Fi connected\nGetting your Art Computer ready. This can take a minute.";
  else if(state=="connecting")panel.text="Connecting to the network"+(reason.empty()?"":"\n"+reason);
  else if(state=="join_failed")panel.text="Couldn't connect to Wi-Fi\n"+(reason.empty()?"Please try again.":reason);
  else if(state=="setup_error")panel.text="Setup needs attention\n"+(reason.empty()?"The Art Computer ran into a problem with setup mode. It will keep trying automatically.":reason);
  else if(state=="updating") {
    panel.text="Updating software";
    if(f.contains("progress"))panel.text+="\n"+std::to_string(static_cast<int>(std::round(std::clamp(f["progress"].get<double>(),0.0,100.0))))+"%";
    panel.text+="\nDon't unplug it.";
  } else if(state=="factory_reset")panel.text="Resetting to factory settings\nDon't unplug it.";
  return panel;
}
}
