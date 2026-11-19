#include "ff/device_ui.hpp"
#include "ff/overlay.hpp"
#include <cairo.h>
#include <fontconfig/fontconfig.h>
#include <pango/pangocairo.h>
#include <pango/pangofc-fontmap.h>
#include <qrencode.h>
#include <algorithm>
#include <cmath>
#include <filesystem>
#include <memory>

namespace ff {
namespace {
Json label_metadata(const Occurrence& o) {
  for(auto key:{"resolvedManifest","inlineManifest"})
    if(o.item.contains(key)&&o.item[key].contains("metadata"))return o.item[key]["metadata"];
  return Json::object();
}
std::string escape(const std::string& text) {
  auto value=g_markup_escape_text(text.c_str(),text.size());
  std::string result(value);g_free(value);return result;
}
struct Canvas {
  cairo_surface_t* surface;
  cairo_t* cr;
  Canvas(int width,int height):surface(cairo_image_surface_create(CAIRO_FORMAT_ARGB32,width,height)),cr(cairo_create(surface)) {
    require(cairo_surface_status(surface)==CAIRO_STATUS_SUCCESS,"graphics_error","Cannot allocate device UI");
  }
  ~Canvas(){cairo_destroy(cr);cairo_surface_destroy(surface);}
  UIRaster pixels() {
    cairo_surface_flush(surface);UIRaster result;
    result.width=cairo_image_surface_get_width(surface);result.height=cairo_image_surface_get_height(surface);
    result.rgba.resize(size_t(result.width)*result.height*4);
    auto source=cairo_image_surface_get_data(surface);auto stride=cairo_image_surface_get_stride(surface);
    for(int y=0;y<result.height;++y)for(int x=0;x<result.width;++x){
      auto in=source+y*stride+x*4;auto out=result.rgba.data()+(size_t(y)*result.width+x)*4;
      out[0]=in[2];out[1]=in[1];out[2]=in[0];out[3]=in[3];
    }
    return result;
  }
};
struct Text {
  PangoLayout* layout;
  int width=0,height=0;
  Text(cairo_t* cr,const std::string& markup,double size,double line_height,int max_width,bool center) {
    layout=pango_cairo_create_layout(cr);auto font=pango_font_description_from_string("PP Mori");
    // CSS uses pixels, while Pango's normal font-description size uses points.
    pango_font_description_set_absolute_size(font,size*PANGO_SCALE);
    pango_layout_set_font_description(layout,font);pango_font_description_free(font);
    pango_layout_set_width(layout,max_width*PANGO_SCALE);pango_layout_set_wrap(layout,PANGO_WRAP_WORD_CHAR);
    pango_layout_set_alignment(layout,center?PANGO_ALIGN_CENTER:PANGO_ALIGN_LEFT);
    pango_layout_set_markup(layout,markup.c_str(),markup.size());
    auto attrs=pango_layout_get_attributes(layout);attrs=attrs?pango_attr_list_copy(attrs):pango_attr_list_new();
    pango_attr_list_insert(attrs,pango_attr_line_height_new_absolute(size*line_height*PANGO_SCALE));
    pango_layout_set_attributes(layout,attrs);pango_attr_list_unref(attrs);
    pango_layout_get_pixel_size(layout,&width,&height);
  }
  ~Text(){g_object_unref(layout);}
  void paint(cairo_t* cr,double x,double y){cairo_move_to(cr,x,y);pango_cairo_show_layout(cr,layout);}
};
struct Block {std::string markup;double size,line_height,gap;};
void rounded(cairo_t* cr,double x,double y,double size,double radius) {
  constexpr double pi=3.141592653589793;
  cairo_new_sub_path(cr);
  cairo_arc(cr,x+size-radius,y+radius,radius,-pi/2,0);
  cairo_arc(cr,x+size-radius,y+size-radius,radius,0,pi/2);
  cairo_arc(cr,x+radius,y+size-radius,radius,pi/2,pi);
  cairo_arc(cr,x+radius,y+radius,radius,pi,pi*1.5);cairo_close_path(cr);
}
UIRaster panel(std::vector<Block> before,const std::string& qr,std::vector<Block> after,int width,int height) {
  require(width>0&&height>0&&width<=16384&&height<=16384,"graphics_error","Invalid UI dimensions");
  const double edge=std::min(width,height),qr_size=std::min(edge*.296,400.0),padding=edge*.022;
  const int text_width=std::max(1,int(std::min(width*.86,edge*.96)));
  Canvas canvas(width,height);cairo_set_source_rgba(canvas.cr,0,0,0,.82);cairo_paint(canvas.cr);
  std::vector<std::unique_ptr<Text>> layouts;double total=0;
  auto measure=[&](const std::vector<Block>& blocks){for(const auto& block:blocks){
    auto t=std::make_unique<Text>(canvas.cr,block.markup,block.size,block.line_height,text_width,true);
    total+=block.gap+t->height;layouts.push_back(std::move(t));
  }};
  measure(before);if(!qr.empty())total+=height*.05+qr_size+padding*2;measure(after);
  // Dense recovery copy must remain on-screen on square and portrait outputs.
  // Uniformly reduce only when the CSS-sized stack exceeds its 88% safe area.
  const double scale=std::min(1.0,height*.88/std::max(1.0,total));
  cairo_translate(canvas.cr,width*.5,(height-total*scale)*.5);cairo_scale(canvas.cr,scale,scale);
  cairo_set_source_rgb(canvas.cr,1,1,1);size_t i=0;double y=0;
  auto paint=[&](const std::vector<Block>& blocks){for(const auto& block:blocks){
    y+=block.gap;layouts[i]->paint(canvas.cr,-text_width*.5,y);y+=layouts[i++]->height;
  }};
  paint(before);
  if(!qr.empty()){
    auto code=QRcode_encodeString(qr.c_str(),0,QR_ECLEVEL_L,QR_MODE_8,1);
    require(code!=nullptr,"graphics_error","QR payload cannot be encoded");
    y+=height*.05;const double box=qr_size+padding*2;
    cairo_set_source_rgb(canvas.cr,1,1,1);rounded(canvas.cr,-box*.5,y,box,edge*.015);cairo_fill(canvas.cr);
    const double cell=qr_size/(code->width+4); // qrcode.react marginSize=2.
    cairo_set_antialias(canvas.cr,CAIRO_ANTIALIAS_NONE);cairo_set_source_rgb(canvas.cr,0,0,0);
    for(int row=0;row<code->width;++row)for(int col=0;col<code->width;++col)if(code->data[row*code->width+col]&1){
      cairo_rectangle(canvas.cr,-qr_size*.5+(col+2)*cell,y+padding+(row+2)*cell,cell,cell);
    }
    cairo_fill(canvas.cr);cairo_set_antialias(canvas.cr,CAIRO_ANTIALIAS_DEFAULT);QRcode_free(code);
    y+=box;cairo_set_source_rgb(canvas.cr,1,1,1);
  }
  paint(after);return canvas.pixels();
}
}
PointerRaster render_pointer(int size) {
  Canvas canvas(size, size);
  const int hotspot = size / 8;
  cairo_translate(canvas.cr, hotspot, hotspot);
  cairo_scale(canvas.cr, size / 32.0, size / 32.0);

  // The arrow tip is the input hotspot. Yellow echoes the old player's dot,
  // and the dark edge keeps the shape visible on pale artwork.
  cairo_move_to(canvas.cr, 0, 0);
  cairo_line_to(canvas.cr, 0, 23);
  cairo_line_to(canvas.cr, 6, 17);
  cairo_line_to(canvas.cr, 10, 25);
  cairo_line_to(canvas.cr, 15, 23);
  cairo_line_to(canvas.cr, 11, 15);
  cairo_line_to(canvas.cr, 19, 15);
  cairo_close_path(canvas.cr);
  cairo_set_source_rgb(canvas.cr, 1, .91, .3);
  cairo_fill_preserve(canvas.cr);
  cairo_set_line_width(canvas.cr, 2.5);
  cairo_set_line_join(canvas.cr, CAIRO_LINE_JOIN_ROUND);
  cairo_set_source_rgb(canvas.cr, .08, .08, .08);
  cairo_stroke(canvas.cr);
  return {canvas.pixels(), hotspot};
}
void load_device_fonts(const std::string& directory) {
  auto config=FcConfigGetCurrent();
  for(auto file:{"PPMori-Regular.ttf","PPMori-Bold.ttf","PPMori-BoldItalic.ttf"}){
    auto path=(std::filesystem::path(directory)/file).string();
    require(FcConfigAppFontAddFile(config,reinterpret_cast<const FcChar8*>(path.c_str())),"font_unavailable","Cannot load bundled PP Mori face: "+path);
  }
  // Attach the application config explicitly. Use SFNT/TTF assets: Fontconfig
  // scans the WOFF originals, but Pango 1.58's shaping path substitutes them.
  // The glyph-run test checks the actual painted face, not just FcFontMatch.
  auto map=pango_cairo_font_map_get_default();
  pango_fc_font_map_set_config(PANGO_FC_FONT_MAP(map),config);
  pango_fc_font_map_config_changed(PANGO_FC_FONT_MAP(map));
}
UIRaster render_message(const std::string& text,int width,int height,bool full) {
  if(!full){
    const double scale=std::min(width,height)/720.0,padding=10*scale;
    Canvas measure(1,1);Text layout(measure.cr,escape(text),16*scale,1.4,std::max(1,int(620*scale)),false);
    Canvas canvas(std::max(1,int(std::ceil(layout.width+padding*2))),std::max(1,std::min(height,int(std::ceil(layout.height+padding*2)))));
    cairo_set_source_rgb(canvas.cr,1,1,1);cairo_paint(canvas.cr);cairo_set_source_rgb(canvas.cr,0,0,0);layout.paint(canvas.cr,padding,padding);
    return canvas.pixels();
  }
  return panel({{escape(text),std::clamp(std::min(width,height)*.045,30.0,62.0),1.14,0}},"",{},width,height);
}
UIRaster render_panel(const Json& overlay,int width,int height) {
  const double edge=std::min(width,height),title=std::clamp(edge*.07,42.0,92.0),subtitle=std::clamp(edge*.045,30.0,62.0);
  const auto fields=overlay.value("fields",Json::object());auto state=overlay.value("state","");
  const auto content=overlay_panel(overlay);std::vector<Block> before,after;
  auto add=[&](const std::string& text,double size,double gap){before.push_back({text,size,size==title?1.08:1.14,gap});};
  if(overlay.value("owner","")=="setup"&&state=="claim_qr"){
    add("Pair with the Feral File app",title,0);
    auto name=fields.value("device_name","");auto name_start=name.find_first_not_of(" \t\r\n");
    name=name_start==std::string::npos?"":name.substr(name_start,name.find_last_not_of(" \t\r\n")-name_start+1);
    add("Connect your phone to the same Wi-Fi as this Art Computer.",subtitle,height*.04);
    add("Then open the app and look for "+(name.empty()?"this Art Computer":"<b>"+escape(name)+"</b>")+", or scan this code.",std::clamp(edge*.032,24.0,44.0),height*.03);
    after.push_back({"<span foreground='#BBBBBB'>Keep this code private: it can be used to pair with this Art Computer.</span>",std::clamp(edge*.025,20.0,36.0),1.2,height*.04});
  }else if(overlay.value("owner","")=="pairing"){
    auto browser=fields.value("browserName","the browser");if(browser.empty())browser="the browser";
    if(state=="pairing_code"){
      add("Pairing code",title,0);add(escape(fields.value("pairingCode","")),std::clamp(edge*.13,72.0,168.0),height*.06);
      add("Enter it on the website that wants to play art on this Art Computer.",subtitle,height*.04);
    }else if(state=="creating_token")add("Connecting "+escape(browser)+" to this Art Computer…",title,0);
    else {add(escape(browser)+" wants to play art on this Art Computer.",title,0);add("Open the Feral File app, go to Settings &gt; Art Computer, and approve the request.",subtitle,height*.04);}
  }else{
    auto split=content.text.find('\n');add(escape(content.text.substr(0,split)),title,0);
    if(split!=std::string::npos){
      auto block=Block{escape(content.text.substr(split+1)),state=="softap_qr"?std::clamp(edge*.03,22.0,42.0):subtitle,1.14,height*(state=="softap_qr"?.03:.04)};
      (state=="softap_qr"?after:before).push_back(block);
    }
  }
  return panel(std::move(before),content.qr,std::move(after),width,height);
}
bool has_tombstone(const Occurrence& o) {
  const auto metadata=label_metadata(o);
  if(metadata.contains("title"))return !metadata["title"].get_ref<const std::string&>().empty();
  if(o.item.contains("metadata")&&o.item["metadata"].contains("title"))return !o.item["metadata"]["title"].get_ref<const std::string&>().empty();
  return o.item.contains("title")&&!o.item["title"].get_ref<const std::string&>().empty();
}
UIRaster render_tombstone(const Occurrence& o,int width,int height,bool timed) {
  const double scale=std::min(width,height)/720.0,padding=10*scale,size=16*scale;
  auto legacy=o.item.value("metadata",Json::object());
  auto metadata=label_metadata(o);
  auto title=metadata.value("title",legacy.value("title",o.item.value("title","")));if(title.empty())return {};
  std::string artists;auto people=metadata.value("artists",legacy.value("artists",Json::array()));
  if(people.is_array())for(const auto& person:people){auto name=person.is_string()?person.get<std::string>():person.value("name","");if(!name.empty()){if(!artists.empty())artists+=", ";artists+=name;}}
  Canvas measure(1,1);const int max_width=std::max(1,int(640*scale-padding*2));
  std::vector<std::string> blocks{(artists.empty()?"":escape(artists)+"\n")+"<b><i>"+escape(title)+"</i></b>"};
  if(!o.playlist_title.empty())blocks.push_back(escape(o.playlist_title));
  if(!o.curator.empty())blocks.push_back("<span foreground='#A0A0A0'>Curated by: "+escape(o.curator)+"</span>");
  std::vector<std::unique_ptr<Text>> layouts;int text_width=0;double total=0;
  for(const auto& block:blocks){auto t=std::make_unique<Text>(measure.cr,block,size,1.4,max_width,false);text_width=std::max(text_width,t->width);total+=t->height;layouts.push_back(std::move(t));}
  const double timer=timed?std::max(1.0,scale):0;
  total+=padding*2+padding*(blocks.size()-1)+(timed?timer+padding:0);
  // Bound hostile/very long metadata. Normal labels use content-hugging width.
  Canvas canvas(std::max(1,int(std::ceil(text_width+padding*2))),std::max(1,std::min(height,int(std::ceil(total)))));
  cairo_set_source_rgb(canvas.cr,1,1,1);cairo_paint(canvas.cr);cairo_set_source_rgb(canvas.cr,0,0,0);
  double y=padding+(timed?timer+padding:0);
  for(auto& layout:layouts){layout->paint(canvas.cr,padding,y);y+=layout->height+padding;}
  auto result=canvas.pixels();
  if(timed){result.timer_x=std::round(padding);result.timer_y=std::round(padding);result.timer_width=std::max(1,result.width-2*result.timer_x);result.timer_height=std::max(1,int(std::round(timer)));}
  return result;
}
}
