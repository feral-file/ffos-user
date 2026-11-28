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
 std::array<double,4> inset{};
 const auto it=display.find("margin");
 if(it!=display.end()){
   if(it->is_number()){
     double px=it->get<double>();
     require(std::isfinite(px)&&px>=0&&px<=86400000,"playlist_invalid","Invalid pixel margin");
     inset.fill(px);
   }else{
     require(it->is_string() && it->get_ref<const std::string&>().size()<=96,"playlist_invalid","Invalid CSS padding");
     std::istringstream stream(it->get_ref<const std::string&>());
     std::vector<std::string> parts;
     for(std::string part;stream>>part;)parts.push_back(part);
     require(!parts.empty() && parts.size()<=4,"playlist_invalid","Expected one to four padding lengths");
     const size_t sides[]={0,parts.size()>1?1u:0u,parts.size()>2?2u:0u,parts.size()==4?3u:parts.size()>1?1u:0u};
     for(size_t side=0;side<4;++side){
       const auto& token=parts[sides[side]];size_t n=0;double value;
       try{value=std::stod(token,&n);}catch(const std::exception&){throw Error("playlist_invalid","Invalid CSS padding length");}
       require(std::isfinite(value)&&value>=0&&value<=16384,"playlist_invalid","Invalid CSS padding magnitude");
       const auto unit=token.substr(n);
       if(unit=="px" || (unit.empty() && value==0))inset[side]=value;
       else if(unit=="em" || unit=="rem")inset[side]=value*16;
       else if(unit=="vw")inset[side]=value*width/100;
       else if(unit=="vh")inset[side]=value*height/100;
       else if(unit=="vmin")inset[side]=value*std::min(width,height)/100;
       else if(unit=="vmax")inset[side]=value*std::max(width,height)/100;
       else if(unit=="%"){
         require(value<=100,"playlist_invalid","Padding percentage exceeds 100");
         // ff-player translates a single percentage into vh vertically and vw horizontally.
         inset[side]=value*(parts.size()==1 && side%2==0?height:width)/100;
       }else throw Error("playlist_invalid","Unsupported CSS padding unit");
     }
   }
 }
 // Scale excessive opposing insets together to retain a drawable pixel.
 auto constrain=[](double& a,double& b,int extent){
   if(a+b>=extent){double scale=(extent-1)/(a+b);a*=scale;b*=scale;}
 };
 constrain(inset[1],inset[3],width);constrain(inset[0],inset[2],height);
 return {int(inset[3]),int(inset[0]),width-int(inset[1])-int(inset[3]),height-int(inset[0])-int(inset[2])};
}
}
