#include "ff/server.hpp"
#include <filesystem>
#include <cstring>
#include <iostream>
#include <fstream>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

namespace {
void check(bool value,const char* message){if(!value)throw std::runtime_error(message);}
int connect_to(const std::string& path){
  int fd=socket(AF_UNIX,SOCK_STREAM,0);check(fd>=0,"create client socket");
  sockaddr_un address{};address.sun_family=AF_UNIX;std::memcpy(address.sun_path,path.c_str(),path.size()+1);
  check(connect(fd,reinterpret_cast<sockaddr*>(&address),sizeof(address))==0,"connect client");return fd;
}
}
int main(int argc,char** argv){
  auto directory=std::filesystem::temp_directory_path()/ ("ff-wire-"+ff::uuid());
  try{
    std::filesystem::create_directory(directory);chmod(directory.c_str(),0700);
    ff::Store store((directory/"state.sqlite").string());ff::Playback playback(store);ff::Protocol protocol(playback);
    int64_t now=1000;auto path=(directory/"socket").string();
    ff::Server server(path,protocol,[&]{return now;});
    check(argc==2,"shared fixture path required");std::ifstream fixture_file(argv[1]);auto fixtures=ff::Json::parse(fixture_file);
    for(const auto& fixture:fixtures["frames"]){
      auto hex=fixture["hex"].get<std::string>();std::string wire;
      for(size_t offset=0;offset<hex.size();offset+=2)wire.push_back(static_cast<char>(std::stoul(hex.substr(offset,2),nullptr,16)));
      int peer=connect_to(path);server.poll();check(send(peer,wire.data(),wire.size(),0)==static_cast<ssize_t>(wire.size()),"fixture send");server.poll();
      char reply[8192];auto size=recv(peer,reply,sizeof reply,MSG_DONTWAIT);
      if(fixture.contains("error"))check(size==0,"invalid shared framing fixture must close before allocation");
      else {
        check(size>4,"valid shared frame must reach request validation");
        check(ff::frame(ff::parse(fixture["body"].get<std::string>()))==wire,"C++ frame bytes match shared fixture");
      }
      close(peer);server.poll();
    }
    int slow=connect_to(path);server.poll();
    check(send(slow,"\0",1,0)==1,"send first framing byte");server.poll();
    now+=4999;check(send(slow,"\0",1,0)==1,"dribble next byte");server.poll();
    now+=1;server.poll();char buffer[8192];check(recv(slow,buffer,sizeof buffer,MSG_DONTWAIT)==0,"dribbling must not extend handshake deadline");close(slow);
    int good=connect_to(path);server.poll();
    auto hello=ff::frame({{"v",1},{"id","h"},{"method","hello"},{"params",{{"client","socket-test"},{"versions",{1}}}}});
    for(char byte:hello){check(send(good,&byte,1,0)==1,"fragmented send");server.poll();}
    check(recv(good,buffer,sizeof buffer,MSG_DONTWAIT)>4,"fragmented hello response");
    now+=60000;server.poll();check(recv(good,buffer,sizeof buffer,MSG_DONTWAIT)<0,"negotiated idle peer remains open");
    check(send(good,"\0",1,0)==1,"start incomplete post-negotiation frame");server.poll();
    now+=4000;check(send(good,"\0",1,0)==1,"dribble post-negotiation byte");server.poll();
    now+=1000;server.poll();check(recv(good,buffer,sizeof buffer,MSG_DONTWAIT)==0,"partial-frame deadline is absolute");close(good);
    int large=connect_to(path);server.poll();const char oversized[]={0x7f,0,0,0};
    check(send(large,oversized,sizeof oversized,0)==4,"oversized header send");server.poll();
    check(recv(large,buffer,sizeof buffer,MSG_DONTWAIT)==0,"oversized header closes before allocation");close(large);
    std::filesystem::remove_all(directory);std::cout<<"socket framing, absolute deadlines and idle peer checks passed\n";
  }catch(const std::exception& error){std::filesystem::remove_all(directory);std::cerr<<error.what()<<'\n';return 1;}
}
