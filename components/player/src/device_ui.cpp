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
const std::string* text_field(const Json& data, const char* key) {
  auto it = data.find(key);
  if (it == data.end() || !it->is_string()) return nullptr;
  const auto& value = it->get_ref<const std::string&>();
  return value.empty() ? nullptr : &value;
}
const std::string* artist_name(const Json& person) {
  // The native playlist contract also accepts legacy string-form artists.
  if (!person.is_string()) return text_field(person, "name");
  const auto& value = person.get_ref<const std::string&>();
  return value.empty() ? nullptr : &value;
}
struct Label {
  const std::string* title = nullptr;
  const Json* artists = nullptr;
};
Label metadata_label(const Json& data) {
  Label label{text_field(data, "title")};
  auto artists = data.find("artists");
  if (artists != data.end() && artists->is_array() &&
      std::any_of(artists->begin(), artists->end(), artist_name))
    label.artists = &*artists;
  return label;
}
Label tombstone_label(const Occurrence& o) {
  Label label;
  // The first manifest with usable label data owns both fields. Only the
  // legacy inline metadata falls through per field, as on the shipping wall.
  for (auto key : {"resolvedManifest", "inlineManifest"}) {
    auto manifest = o.item.find(key);
    if (manifest == o.item.end() || !manifest->contains("metadata")) continue;
    label = metadata_label((*manifest)["metadata"]);
    if (label.title || label.artists) break;
  }
  if ((!label.title || !label.artists) && o.item.contains("metadata")) {
    auto legacy = metadata_label(o.item["metadata"]);
    if (!label.title) label.title = legacy.title;
    if (!label.artists) label.artists = legacy.artists;
  }
  if (!label.title) label.title = text_field(o.item, "title");
  return label;
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
  double css_height=0;
  Text(cairo_t* cr,const std::string& markup,double size,double line_height,int max_width,bool center) {
    layout=pango_cairo_create_layout(cr);auto font=pango_font_description_from_string("PP Mori");
    // CSS keeps fractional glyph advances. Rounding each character pushes the
    // claim title over its 86vw limit on a portrait 1080px display.
    pango_context_set_round_glyph_positions(pango_layout_get_context(layout), false);
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
    // Stack CSS line boxes before rasterization. Rounding each label separately
    // adds a pixel to the plaque and shifts text and QR frames in long stacks.
    css_height=pango_layout_get_line_count(layout)*size*line_height;
  }
  ~Text(){g_object_unref(layout);}
  void paint(cairo_t* cr,double x,double y){cairo_move_to(cr,x,y);pango_cairo_show_layout(cr,layout);}
};
struct Block {
  std::string markup;
  double size, line_height, gap;
  double width_fraction = .96, opacity = 1;
};
void rounded(cairo_t* cr,double x,double y,double width,double height,double radius) {
  constexpr double pi=3.141592653589793;
  cairo_new_sub_path(cr);
  cairo_arc(cr,x+width-radius,y+radius,radius,-pi/2,0);
  cairo_arc(cr,x+width-radius,y+height-radius,radius,0,pi/2);
  cairo_arc(cr,x+radius,y+height-radius,radius,pi/2,pi);
  cairo_arc(cr,x+radius,y+radius,radius,pi,pi*1.5);cairo_close_path(cr);
}
UIRaster panel(std::vector<Block> before,const std::string& qr,std::vector<Block> after,int width,int height) {
  require(width>0&&height>0&&width<=16384&&height<=16384,"graphics_error","Invalid UI dimensions");
  const double edge=std::min(width,height),qr_size=std::min(edge*.296,400.0),padding=edge*.022;
  auto text_width = [&](const Block& block) { return std::max(1, int(std::min(width*.86, edge*block.width_fraction))); };
  Canvas canvas(width,height);cairo_set_source_rgba(canvas.cr,0,0,0,.82);cairo_paint(canvas.cr);
  std::vector<std::unique_ptr<Text>> layouts;double total=0;
  auto measure=[&](const std::vector<Block>& blocks){for(const auto& block:blocks){
    auto t=std::make_unique<Text>(canvas.cr,block.markup,block.size,block.line_height,text_width(block),true);
    total+=block.gap+t->css_height;layouts.push_back(std::move(t));
  }};
  measure(before);if(!qr.empty())total+=height*.05+qr_size+padding*2;measure(after);
  // Dense recovery copy must remain on-screen on square and portrait outputs.
  // Uniformly reduce only when the CSS-sized stack exceeds its 88% safe area.
  const double scale=std::min(1.0,height*.88/std::max(1.0,total));
  cairo_translate(canvas.cr,width*.5,(height-total*scale)*.5);cairo_scale(canvas.cr,scale,scale);
  cairo_set_source_rgb(canvas.cr,1,1,1);size_t i=0;double y=0;
  auto paint=[&](const std::vector<Block>& blocks){for(const auto& block:blocks){
    cairo_set_source_rgba(canvas.cr,1,1,1,block.opacity);
    y+=block.gap;layouts[i]->paint(canvas.cr,-text_width(block)*.5,y);y+=layouts[i++]->css_height;
  }};
  paint(before);
  if(!qr.empty()){
    auto code=QRcode_encodeString(qr.c_str(),0,QR_ECLEVEL_L,QR_MODE_8,1);
    require(code!=nullptr,"graphics_error","QR payload cannot be encoded");
    y+=height*.05;const double box=qr_size+padding*2;
    cairo_set_source_rgb(canvas.cr,1,1,1);rounded(canvas.cr,-box*.5,y,box,box,edge*.015);cairo_fill(canvas.cr);
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
PointerRaster render_pointer(int size, bool pressed) {
  Canvas canvas(size, size);
  const int hotspot = size / 4;
  cairo_translate(canvas.cr, hotspot, hotspot);
  cairo_scale(canvas.cr, size / 32.0, size / 32.0);
  cairo_set_line_join(canvas.cr, CAIRO_LINE_JOIN_ROUND);
  // The open notch leaves the art visible. Both silhouettes point to the
  // same pixel so changing pose never moves the apparent click target.
  const double reach = pressed ? 11 : 17;
  cairo_move_to(canvas.cr, 0, 0);
  cairo_line_to(canvas.cr, reach, reach * .55);
  cairo_line_to(canvas.cr, reach * .48, reach * .64);
  cairo_line_to(canvas.cr, reach * .29, reach);
  cairo_close_path(canvas.cr);
  cairo_set_source_rgb(canvas.cr, 1, .91, .3);
  cairo_fill_preserve(canvas.cr);
  cairo_set_line_width(canvas.cr, 1.8);
  cairo_set_source_rgb(canvas.cr, .08, .08, .08);
  cairo_stroke(canvas.cr);
  if (pressed) {
    cairo_arc(canvas.cr, 0, 0, 5, 0, 6.283185307);
    cairo_set_line_width(canvas.cr, 3);
    cairo_stroke_preserve(canvas.cr);
    cairo_set_source_rgb(canvas.cr, 1, .98, .88);
    cairo_set_line_width(canvas.cr, 1.4);
    cairo_stroke(canvas.cr);
  }
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
UIRaster render_loading_label() {
  Canvas measure(1,1);
  Text label(measure.cr,"Loading...",32,1.2,480,false);
  Canvas canvas(label.width,label.height+4);
  cairo_set_source_rgb(canvas.cr,1,1,1);
  label.paint(canvas.cr,0,0);
  cairo_set_source_rgb(canvas.cr,46/255.,46/255.,46/255.);
  cairo_rectangle(canvas.cr,0,label.height,label.width,4);
  cairo_fill(canvas.cr);
  return canvas.pixels();
}
UIRaster render_message(const std::string& text,int width,int height,bool full) {
  if(!full){
    const double scale=std::min(width,height)/720.0,padding=10*scale;
    Canvas measure(1,1);Text layout(measure.cr,escape(text),16*scale,1.4,std::max(1,int(620*scale)),false);
    Canvas canvas(std::max(1,int(std::ceil(layout.width+padding*2))),std::max(1,std::min(height,int(std::ceil(layout.height+padding*2)))));
    cairo_set_source_rgb(canvas.cr,1,1,1);cairo_paint(canvas.cr);cairo_set_source_rgb(canvas.cr,0,0,0);layout.paint(canvas.cr,padding,padding);
    return canvas.pixels();
  }
  // MessageModal at screenRatio=1 has a centered 80%-wide charcoal card.
  Canvas canvas(width,height);
  cairo_set_source_rgba(canvas.cr,0,0,0,.80);cairo_paint(canvas.cr);
  const double card_width=width*.8;
  const double padding=std::min({100.,card_width*.25,height*.18});
  const int text_width=std::max(1,int(card_width-2*padding));
  auto split=text.find('\n');
  Text heading(canvas.cr,escape(text.substr(0,split)),36,1.2,text_width,true);
  const auto body_text=split==std::string::npos?"":text.substr(split+1);
  Text body(canvas.cr,escape(body_text),24,1.2,text_width,true);
  const double gap=body_text.empty()?0:30;
  const double stack_height=heading.height+gap+(body_text.empty()?0:body.height);
  const double card_height=std::min(double(height),stack_height+2*padding);
  const double top=(height-card_height)/2;
  cairo_set_source_rgb(canvas.cr,46/255.,46/255.,46/255.);
  rounded(canvas.cr,(width-card_width)/2,top,card_width,card_height,std::min(20.,card_height/2));
  cairo_fill(canvas.cr);cairo_set_source_rgb(canvas.cr,1,1,1);
  const double x=(width-text_width)/2.,y=std::max(top,(height-stack_height)/2.);
  heading.paint(canvas.cr,x,y);
  if(!body_text.empty())body.paint(canvas.cr,x,y+heading.height+gap);
  return canvas.pixels();
}
UIRaster render_panel(const Json& overlay,int width,int height) {
  const double edge = std::min(width, height);
  const auto content = overlay_panel(overlay);
  auto blocks = [&](const std::vector<PanelLine>& lines) {
    std::vector<Block> result;
    for (const auto& line : lines) {
      using enum PanelStyle;
      const auto& text = line.markup;
      switch (line.style) {
        case title: result.push_back({text, std::clamp(edge*.07, 42.0, 92.0), 1.08, 0}); break;
        case subtitle: result.push_back({text, std::clamp(edge*.045, 30.0, 62.0), 1.14, height*.04, .89}); break;
        case compact: result.push_back({text, std::clamp(edge*.03, 22.0, 42.0), 1.14, height*.03}); break;
        case code: result.push_back({text, std::clamp(edge*.13, 72.0, 168.0), 1, height*.06}); break;
        case step: result.push_back({text, std::clamp(edge*.032, 24.0, 44.0), 1.14, height*.03}); break;
        case note: result.push_back({text, std::clamp(edge*.026, 20.0, 36.0), 1.2, height*.04, .96, .72}); break;
      }
    }
    return result;
  };
  return panel(blocks(content.before_qr), content.qr, blocks(content.after_qr), width, height);
}
bool has_tombstone(const Occurrence& o) {
  return tombstone_label(o).title != nullptr;
}
UIRaster render_tombstone(const Occurrence& o,int width,int height,bool timed) {
  const double scale=std::min(width,height)/720.0,padding=10*scale,size=16*scale;
  const auto label = tombstone_label(o);
  if (!label.title) return {};
  std::string artists;
  if (label.artists) {
    for (const auto& person : *label.artists) {
      if (auto name = artist_name(person)) {
        if (!artists.empty()) artists += ", ";
        artists += *name;
      }
    }
  }
  Canvas measure(1,1);const int max_width=std::max(1,int(640*scale-padding*2));
  std::vector<std::string> blocks{(artists.empty()?"":escape(artists)+"\n")+"<b><i>"+escape(*label.title)+"</i></b>"};
  if(!o.curator.empty())blocks.push_back("<span foreground='#A0A0A0'>Curated by: "+escape(o.curator)+"</span>");
  std::vector<std::unique_ptr<Text>> layouts;int text_width=0;double total=0;
  for(const auto& block:blocks){auto t=std::make_unique<Text>(measure.cr,block,size,1.4,max_width,false);text_width=std::max(text_width,t->width);total+=t->css_height;layouts.push_back(std::move(t));}
  const double timer=timed?std::max(1.0,scale):0;
  total+=padding*2+padding*(blocks.size()-1)+(timed?timer+padding:0);
  // Bound hostile/very long metadata. Normal labels use content-hugging width.
  Canvas canvas(std::max(1,int(std::ceil(text_width+padding*2))),std::max(1,std::min(height,int(std::ceil(total)))));
  cairo_set_source_rgb(canvas.cr,1,1,1);cairo_paint(canvas.cr);cairo_set_source_rgb(canvas.cr,0,0,0);
  double y=padding+(timed?timer+padding:0);
  for(auto& layout:layouts){layout->paint(canvas.cr,padding,y);y+=layout->css_height+padding;}
  auto result=canvas.pixels();
  if(timed){result.timer_x=std::round(padding);result.timer_y=std::round(padding);result.timer_width=std::max(1,result.width-2*result.timer_x);result.timer_height=std::max(1,int(std::round(timer)));}
  return result;
}
}
