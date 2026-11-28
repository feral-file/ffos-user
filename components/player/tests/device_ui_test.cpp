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
    item.playlist["links"] = ff::Json::array({{{"url","mailto:test@example.org"}},{{"url","https://example.org/"+std::string(513,'a')}}});
    check(ff::render_title_card(item,1920,1080).rgba==landscape.rgba,"Invalid and long links omit the code");
    item.playlist["links"].push_back({{"url","https://example.org/episode?x=1&y=2"}});
    const auto linked=ff::render_title_card(item,1920,1080);
    check(linked.rgba!=landscape.rgba,"A playlist link adds a code");
    item.playlist["links"].push_back({{"url","https://other.example/"}});
    check(ff::render_title_card(item,1920,1080).rgba==linked.rgba,"First usable link wins");
    save(linked,std::string(argv[2])+"/linked-title-card.png");
    {
      // docs/TITLE-CARD.md: a 50-unit margin at 1.5 px per unit, text in
      // #EDEDED, and a borderless code whose top-left finder corner is ink.
      auto at=[&](int x,int y){return linked.rgba[(size_t(y)*1920+x)*4];};
      bool margin_clear=true;
      for(int y=0;y<1080;++y)for(int x=0;x<1920;++x)
        if((x<73||x>=1847||y<73)&&at(x,y)>40)margin_clear=false;
      check(margin_clear,"Title card text and code stay inside the 50-unit margin");
      check(at(1920-75-180+3,1080-140)==0 || at(1920-75-180+3,1080-140)==0xED,"Code is drawn in the text color on black");
      int ink=0;for(int y=700;y<1080;++y)if(at(1920-75-178,y)==0xED)++ink;
      check(ink>40,"The code's left finder edge sits at the right margin, 120 units wide");
    }
    {
      auto collective=item;
      for(int n=0;n<30;++n) collective.item["metadata"]["artists"].push_back("Alexandra Example");
      const auto card=ff::render_title_card(collective,1920,1080);
      int ink=0;for(int y=700;y<1080;++y)if(card.rgba[(size_t(y)*1920+1920-75-178)*4]==0xED)++ink;
      check(ink>40,"Long credits keep the QR finder on screen");
      collective.item["title"]="Another title";
      check(ff::render_title_card(collective,1920,1080).rgba!=card.rgba,"Long credits keep the title visible");
      save(card,std::string(argv[2])+"/collective-title-card.png");
    }
    item.playlist.erase("links");
    item.playlist_title="Another exhibition";
    check(ff::render_title_card(item,1920,1080).rgba!=landscape.rgba,"The exhibition name belongs on the card");
    auto daily = item;
    daily.curator.clear();
    daily.playlist["items"] = ff::Json::array({daily.item});
    daily.playlist_title = "Living Code";
    const auto lone = ff::render_title_card(daily,1920,1080);
    check(lone.rgba != landscape.rgba,"An ordinary lone work omits the exhibition header");
    daily.item["displayAt"] = "2026-11-26T00:00:00Z";
    check(ff::render_title_card(daily,1920,1080).rgba==landscape.rgba,"Today's work retains its scheduled exhibition header");
    daily.playlist_title = "Transparent Grit";
    check(ff::render_title_card(daily,1920,1080).rgba==lone.rgba,"A scheduled lone work does not repeat its title");
    ff::TitleCardClock clock;
    clock.select("playlist",100);
    std::optional<ff::Occurrence> upcoming=item, shown=item, none;
    upcoming->cast_id="scheduled-plantoid";upcoming->item["title"]="Plantoid";
    shown->cast_id="previous-slot";shown->item["title"]="Lately When I Fly";
    check(&clock.artwork(100,upcoming,none)==&upcoming, "Opening card introduces the selection before the first frame");
    check(&clock.artwork(3600100,upcoming,shown)==&shown, "Hourly card belongs to the presented work across casts");
    check(!clock.artwork(3600100,upcoming,none), "An unpresented selection cannot supply an hourly card");
    check(!clock.artwork(3600100,none,shown), "A stopped playlist has no card");
    check(!clock.visible(100,false,false),"Wait for metadata");
    check(clock.visible(300,true,true) && !clock.visible(10300,true,true),"Ten seconds from first readable title");
    clock.select("playlist",10400);
    check(!clock.visible(10400,true,true),"Recast and item changes preserve the hour");
    check(clock.visible(3600100,true,true) && !clock.visible(3610100,true,true),"Hourly card lasts ten seconds");
    clock.select("late-opening",0);
    check(!clock.visible(0,true,false,false) && clock.visible(3595000,true,false), "A covered opening can become readable just before the hour");
    check(clock.visible(3600000,true,false) && &clock.artwork(3600000,upcoming,shown)==&shown,
          "A visible opening card switches ownership at the first hour even while the artwork frame is static");
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
