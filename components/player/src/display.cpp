#include "ff/display.hpp"
#include <algorithm>
#include <cmath>
#include <cctype>
#include <iomanip>
#include <sstream>
#include <array>

namespace ff {
namespace {
const std::pair<std::string_view,uint32_t> names[]={
#include "ff/css_named_colors.inc"
};
double component(const std::string& token,double percent){
 size_t used=0;double value;
 try{value=std::stod(token,&used);}catch(const std::exception&){throw Error("playlist_invalid","Invalid CSS color component");}
 require(std::isfinite(value),"playlist_invalid","Nonfinite CSS color component");
 if(used<token.size()&&token.substr(used)=="%")return value*percent/100;
 require(used==token.size(),"playlist_invalid","Unsupported CSS color component");return value;
}
}
std::string display_color(const std::string& css){
 require(css.size()<=64,"playlist_invalid","CSS color exceeds 64 bytes");
 auto text=css;std::transform(text.begin(),text.end(),text.begin(),[](unsigned char c){return std::tolower(c);});
 auto first=text.find_first_not_of(" \t\r\n"),last=text.find_last_not_of(" \t\r\n");
 require(first!=std::string::npos,"playlist_invalid","Empty background color");text=text.substr(first,last-first+1);
 std::array<double,3> rgb{};double alpha=1;
 if(text=="transparent")alpha=0;
 else if(text[0]=='#'){
  auto hex=text.substr(1);require((hex.size()==3||hex.size()==4||hex.size()==6||hex.size()==8)&&hex.find_first_not_of("0123456789abcdef")==std::string::npos,"playlist_invalid","Invalid hex background color");
  if(hex.size()<5){std::string expanded;for(char c:hex){expanded+=c;expanded+=c;}hex=expanded;}
  for(int i=0;i<3;++i)rgb[i]=std::stoul(hex.substr(i*2,2),nullptr,16);
  if(hex.size()==8)alpha=std::stoul(hex.substr(6,2),nullptr,16)/255.0;
 }else if(auto open=text.find('(');open!=std::string::npos){
  auto kind=text.substr(0,open);require(text.back()==')'&&(kind=="rgb"||kind=="rgba"||kind=="hsl"||kind=="hsla"),"playlist_invalid","Unsupported background color function");
  auto body=text.substr(open+1,text.size()-open-2);for(auto& c:body)if(c==','||c=='/')c=' ';
  std::istringstream input(body);std::vector<std::string> values;for(std::string v;input>>v;)values.push_back(v);
  require(values.size()==3||values.size()==4,"playlist_invalid","Invalid background color arity");
  if(values.size()==4)alpha=std::clamp(component(values[3],1),0.0,1.0);
  if(kind=="rgb"||kind=="rgba")for(int i=0;i<3;++i)rgb[i]=std::clamp(component(values[i],255),0.0,255.0);
  else{
   auto hue=values[0];double unit=1;
   for(auto [suffix,multiplier]:{std::pair{"deg",1.0},std::pair{"grad",.9},std::pair{"rad",180.0/3.141592653589793},std::pair{"turn",360.0}})
    if(hue.ends_with(suffix)){hue.resize(hue.size()-std::string_view(suffix).size());unit=multiplier;break;}
   require(values[1].ends_with('%')&&values[2].ends_with('%'),"playlist_invalid","HSL saturation/lightness must be percentages");
   require(!hue.ends_with('%'),"playlist_invalid","Hue cannot be a percentage");
   double h=std::fmod(component(hue,360)*unit,360);if(h<0)h+=360;
   const double s=std::clamp(component(values[1],1),0.0,1.0),l=std::clamp(component(values[2],1),0.0,1.0),chroma=(1-std::abs(2*l-1))*s,x=chroma*(1-std::abs(std::fmod(h/60,2)-1)),m=l-chroma/2;
   if(h<60)rgb={chroma,x,0};else if(h<120)rgb={x,chroma,0};else if(h<180)rgb={0,chroma,x};else if(h<240)rgb={0,x,chroma};else if(h<300)rgb={x,0,chroma};else rgb={chroma,0,x};
   for(auto& c:rgb)c=(c+m)*255;
  }
 }else{
  auto found=std::lower_bound(std::begin(names),std::end(names),text,[](const auto& pair,const std::string& value){return pair.first<value;});
  require(found!=std::end(names)&&found->first==text,"playlist_invalid","Unsupported background color literal");
  rgb={double((found->second>>16)&255),double((found->second>>8)&255),double(found->second&255)};
 }
 std::ostringstream result;result<<'#'<<std::hex<<std::uppercase<<std::setfill('0');for(auto c:rgb)result<<std::setw(2)<<int(std::round(std::clamp(c*alpha,0.0,255.0)));return result.str();
}
bool hard_pixel_edges(const Json& display,int source_width,int source_height,int width,int height){
  if(display.value("imageRendering","auto")=="pixelated")return true;
  if(source_width<=0 || source_height<=0)return false;
  const double x=double(width)/source_width,y=double(height)/source_height;
  return (display.value("scaling","fit")=="fill"?std::max(x,y):std::min(x,y))>=2.;
}
Bounds display_bounds(const Json& display,int width,int height){
 require(width>0&&height>0,"display_unavailable","Display dimensions must be positive");
 double x=0,y=0;
 if(display.contains("margin")){
  const auto& margin=display["margin"];
  if(margin.is_number())x=y=margin.get<double>();
  else if(margin.is_string()){
   const auto text=margin.get<std::string>();size_t end=0;
   try{auto value=std::stod(text,&end);if(text.ends_with('%')&&end==text.size()-1&&std::isfinite(value)){x=value*width/100;y=value*height/100;}}catch(const std::exception&){}
  }
 }
 int mx=static_cast<int>(std::clamp(x,0.0,(width-1)/2.0));
 int my=static_cast<int>(std::clamp(y,0.0,(height-1)/2.0));
 return {mx,my,width-2*mx,height-2*my};
}
}
