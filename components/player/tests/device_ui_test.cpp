#include "ff/device_ui.hpp"
#include <cairo.h>
#include <fontconfig/fontconfig.h>
#include <pango/pangocairo.h>
#include <filesystem>
#include <iostream>

namespace {
void check(bool ok,const char* message){if(!ok)throw std::runtime_error(message);}
void save(const ff::UIRaster& raster,const std::string& path){
  auto data=raster.rgba;
  for(size_t i=0;i<data.size();i+=4)std::swap(data[i],data[i+2]);
  auto surface=cairo_image_surface_create_for_data(data.data(),CAIRO_FORMAT_ARGB32,raster.width,raster.height,raster.width*4);
  check(cairo_surface_write_to_png(surface,path.c_str())==CAIRO_STATUS_SUCCESS,"Cannot save test raster");cairo_surface_destroy(surface);
}
}
int main(int argc,char** argv){
  try{
    check(argc==3,"Pass font directory and output directory");ff::load_device_fonts(argv[1]);std::filesystem::create_directories(argv[2]);
    auto font_context=pango_font_map_create_context(pango_cairo_font_map_get_default());
    auto description=pango_font_description_from_string("PP Mori");pango_font_description_set_absolute_size(description,92*PANGO_SCALE);
    auto layout=pango_layout_new(font_context);pango_layout_set_font_description(layout,description);
    pango_layout_set_text(layout,"Pair with the Feral File app",-1);
    auto iter=pango_layout_get_iter(layout);auto run=pango_layout_iter_get_run_readonly(iter);
    auto actual=pango_font_describe(run->item->analysis.font);
    int measured_width,measured_height;pango_layout_get_pixel_size(layout,&measured_width,&measured_height);
    std::cout<<"Pango face="<<pango_font_description_get_family(actual)<<" titleWidth="<<measured_width<<'\n';
    check(std::string(pango_font_description_get_family(actual))=="PP Mori","Pango substituted the bundled UI font");
    pango_font_description_free(actual);pango_layout_iter_free(iter);g_object_unref(layout);g_object_unref(font_context);pango_font_description_free(description);
    // A successful raster with substituted DejaVu is not brand-fidelity proof.
    for(auto name:{"PP Mori:style=Regular","PP Mori:style=Bold","PP Mori:style=Bold Italic"}){
      auto pattern=FcNameParse(reinterpret_cast<const FcChar8*>(name));FcConfigSubstitute(nullptr,pattern,FcMatchPattern);FcDefaultSubstitute(pattern);
      FcResult status;auto match=FcFontMatch(nullptr,pattern,&status);FcChar8* family=nullptr;
      check(match&&FcPatternGetString(match,FC_FAMILY,0,&family)==FcResultMatch&&std::string(reinterpret_cast<char*>(family))=="PP Mori","Bundled font was substituted");
      FcPatternDestroy(match);FcPatternDestroy(pattern);
    }
    ff::Occurrence item;item.item={{"title","Transparent Grit"},{"metadata",{{"artists",ff::Json::array({"Dmitri Cherniak"})}}}};item.playlist_title="Living Code";item.curator="Feral File";
    item.playlist={{"items",ff::Json::array({1,2})}};
    const auto landscape=ff::render_title_card(item,1920,1080);
    check(landscape.width==1920 && landscape.height==1080 && landscape.rgba[0]==0 && landscape.rgba[3]==255,"Opaque full-screen title card");
    item.playlist_title="Another exhibition";
    check(ff::render_title_card(item,1920,1080).rgba!=landscape.rgba,"The exhibition name belongs on the card");
    ff::TitleCardClock clock;
    clock.select("playlist",100);
    check(!clock.visible(100,false,false),"Wait for metadata");
    check(clock.visible(300,true,true) && !clock.visible(10300,true,true),"Ten seconds from first readable title");
    clock.select("playlist",10400);
    check(!clock.visible(10400,true,true),"Recast and item changes preserve the hour");
    check(clock.visible(3600100,true,true) && !clock.visible(3610100,true,true),"Hourly card lasts ten seconds");
    clock.select("next",4000000);
    check(clock.visible(4000000,true,false) && clock.visible(4010000,true,false) && !clock.visible(4025000,true,false),"Bound the opening wait for artwork");
    clock.select("covered",5000000);
    check(!clock.visible(5000000,true,true,false) && clock.visible(5040000,true,true),"Setup cannot consume an unseen title");
    check(!clock.visible(5043000,true,true,false) && clock.visible(5053000,true,true) && !clock.visible(5060000,true,true),"An interruption pauses the readable interval");
    const auto moving=ff::render_pointer(40,false),pressed=ff::render_pointer(40,true);
    check(moving.hotspot==pressed.hotspot && moving.image.rgba!=pressed.image.rgba,"Pointer feedback must preserve its hotspot");
    save(moving.image,std::string(argv[2])+"/pointer-moving.png");
    save(pressed.image,std::string(argv[2])+"/pointer-pressed.png");
    const ff::Json pairing={{"owner","pairing"},{"state","pairing_code"},{"fields",{{"pairingCode","123456"}}}};
    save(ff::render_panel(pairing,1920,1080),std::string(argv[2])+"/pairing.png");
    auto notice=ff::render_message("Playback state could not be saved",1920,1080,false);
    check(notice.width<960&&notice.height<200&&notice.rgba[3]==255,"Nonfatal errors must remain compact notices over the retained artwork");
    item.item["title"]="";check(ff::render_title_card(item,1920,1080).rgba.empty(),"Missing title must omit label");
    save(landscape,std::string(argv[2])+"/tombstone.png");
    const ff::Json claim={{"owner","setup"},{"state","claim_qr"},{"fields",{{"url","https://example.org/claim?token=SYNTHETIC-TEST"},{"device_name","My Art Computer"}}}};
    const ff::Json wifi={{"owner","setup"},{"state","softap_qr"},{"fields",{{"ssid","FF1-test:;"},{"password","fake;password"},{"portal_url","http://10.42.0.1/"}}}};
    for(auto [w,h]:{std::pair{1920,1080},std::pair{1080,1920},std::pair{1920,1920},std::pair{3840,2160}}){
      auto result=ff::render_panel(claim,w,h);check(result.width==w&&result.height==h&&result.rgba[3]==209,"Panel dimensions or 82% dimming changed");
      if(w==1080 && h==1920){
        // 3.0.1 places the claim frame at y=972. A wrapped title moves it
        // 41px down; sample the solid white top padding, above the QR cells.
        check(result.rgba[(size_t(982)*w+w/2)*4]==255,"Portrait claim title wrapped and displaced the QR");
      }
      save(result,std::string(argv[2])+"/claim-"+std::to_string(w)+"x"+std::to_string(h)+".png");
      save(ff::render_panel(wifi,w,h),std::string(argv[2])+"/wifi-"+std::to_string(w)+"x"+std::to_string(h)+".png");
    }
    std::cout<<"Device font, title card timing and panel raster checks passed\n";return 0;
  }catch(const std::exception& e){std::cerr<<e.what()<<'\n';return 1;}
}
