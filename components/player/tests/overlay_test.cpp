#include "ff/overlay.hpp"
#include "ff/playback.hpp"
#include <filesystem>
#include <iostream>

namespace {
void check(bool condition,const char* message){if(!condition)throw std::runtime_error(message);}
ff::Json setup(const std::string& state,ff::Json fields=ff::Json::object()){return {{"owner","setup"},{"state",state},{"fields",fields}};}
}
int main(){
 auto directory=std::filesystem::temp_directory_path()/("ff-overlay-test-"+ff::uuid());std::filesystem::create_directory(directory);
 try {
  ff::Store store((directory/"state.sqlite").string());ff::Playback p(store);
  for(auto state:{"scanning","joining","finalizing","connecting","join_failed","setup_error","updating","factory_reset"}){
   auto request=setup(state);p.command("overlay.set",request);
   check(!ff::overlay_panel(request).text.empty(),"Every visible setup state needs native copy");
  }
  auto wifi=setup("softap_qr",{{"ssid","FF1: test;"},{"password","p\\;\""},{"portal_url","http://10.42.0.1/"}});
  p.command("overlay.set",wifi);
  check(ff::overlay_panel(wifi).qr=="WIFI:T:WPA;S:FF1\\: test\\;;P:p\\\\\\;\\\";;","Wi-Fi QR must escape credentials");
  wifi["fields"]["client_attached"]=true;
  check(ff::overlay_panel(wifi).qr=="http://10.42.0.1/","Attached phase must switch the single QR to portal");
  wifi["fields"]["portal_url"]="javascript:alert(1)";
  check(ff::overlay_panel(wifi).qr.starts_with("WIFI:"),"Invalid portal URL must retain join QR");
  auto open=setup("softap_qr",{{"ssid","FF1"}});
  check(ff::overlay_panel(open).qr=="WIFI:T:nopass;S:FF1;;","Open hotspot encoding");
  auto claim=setup("claim_qr",{{"url","https://example.org/claim?a=1&b=2"},{"device_name","My Art Computer"}});
  p.command("overlay.set",claim);auto before=p.overlay();
  for(auto bad:{setup("unknown"),setup("softap_qr"),setup("claim_qr",{{"url",12}}),setup("claim_qr",{{"url","javascript:alert(1)"}}),setup("joining",{{"client_attached","yes"}}),setup("joining",{{"extra",true}})}) {
   bool rejected=false;try{p.command("overlay.set",bad);}catch(const ff::Error&){rejected=true;}
   check(rejected&&p.overlay()==before,"Invalid narration must fail without erasing previous QR");
  }
  p.command("overlay.set",setup("ready"));check(p.overlay().is_null(),"Ready must clear setup UI");
  p.command("overlay.set",{{"owner","pairing"},{"state","pairing_code"}});
  p.command("overlay.set",setup("hidden"));check(p.overlay()["owner"]=="pairing","Setup hide cannot erase another owner");
  p.command("overlay.set",setup("ready"));check(p.overlay()["owner"]=="pairing","Setup ready cannot erase another owner");
  auto long_reason=setup("connecting",{{"reason","  checking & retrying  "}});
  check(ff::overlay_panel(long_reason).text=="Connecting to the network\nchecking & retrying","Prose must stay plain text, neutral and trimmed");
  std::filesystem::remove_all(directory);std::cout<<"Native setup panel, QR and ownership tests passed\n";return 0;
 }catch(const std::exception& e){std::filesystem::remove_all(directory);std::cerr<<e.what()<<'\n';return 1;}
}
