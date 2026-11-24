#include "ff/server.hpp"
#include <arpa/inet.h>
#include <filesystem>
#include <cstring>
#include <iostream>
#include <poll.h>
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

ff::Json receive_result(int fd) {
  uint32_t wire_size = 0;
  check(recv(fd, &wire_size, sizeof wire_size, MSG_WAITALL) == sizeof wire_size, "reply header during swap");
  const size_t size = ntohl(wire_size);
  check(size > 0 && size <= ff::max_frame, "reply length");
  std::string body(size, '\0');
  check(recv(fd, body.data(), size, MSG_WAITALL) == static_cast<ssize_t>(size), "reply body during swap");
  return ff::parse(body).at("result");
}

void send_request(int fd, const ff::Json& request) {
  const auto wire = ff::frame(request);
  check(send(fd, wire.data(), wire.size(), 0) == static_cast<ssize_t>(wire.size()), "send request");
}

void check_slow_swap(ff::Server& server, ff::Playback& playback, const std::string& path) {
  using namespace std::chrono_literals;
  ff::Json items = {{{"id", "first"}, {"source", "https://example.org/a"}},
                    {{"id", "next"}, {"source", "https://example.org/b"}, {"duration",1}}};
  playback.command("playback.replace", {{"playlist", {{"items", items}}}});
  const int fd = connect_to(path);
  timeval timeout{1, 0};
  check(setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof timeout) == 0, "set socket deadline");
  ff::SwapControl control(server, playback, 500ms);
  control.poll();
  ff::Json hello = {{"v",1},{"id","hello"},{"method","hello"},{"params",{{"client","test"},{"versions",{1}}}}};
  ff::Json health = {{"v",1},{"id","health"},{"method","health.get"},{"deadlineMs",ff::wall_ms()+5000}};
  ff::Json snapshot;
  check(control.submit([&] {
    send_request(fd, hello);
    const auto initial = receive_result(fd).at("snapshot");
    send_request(fd, health);
    snapshot = receive_result(fd);
    check(snapshot.at("host") == "responsive" && snapshot.at("instance") == initial.at("instance"), "live health");
    auto signal = health;
    signal.update({{"id","thermal"},{"method","health.set"},{"params",{{"thermal",false}}},
                   {"key","no-change"},{"instance",snapshot.at("instance")},{"generation",snapshot.at("generation")}});
    send_request(fd, signal);
    receive_result(fd);
  }), "queries and repeated health signals preserve the in-flight frame");
  unsigned releases = 0;
  const auto frame_thread = std::this_thread::get_id();
  playback.set_input_handler([&](const ff::Json& event) {
    check(std::this_thread::get_id() == frame_thread && event.at("kind") == "releaseAll",
          "release is delivered on the renderer owner");
    ++releases;
  });
  check(!control.submit([&] {
    send_request(fd, {{"v",1},{"id","select"},{"method","playback.select"},{"params",{{"index",1}}},
                      {"instance",snapshot.at("instance")},{"generation",snapshot.at("generation")},
                      {"deadlineMs",ff::wall_ms()+5000},{"key","select-next"}});
    receive_result(fd);
    send_request(fd, health);
    check(receive_result(fd).at("selected").at("index") == 1, "selection visible before swap returns");
  }), "a superseded frame cannot be acknowledged");
  check(releases == 1, "release queued during selection was delivered");
  // Start the one-second artwork near its deadline. The graphics thread
  // stays inside submit while the control owner advances the playlist.
  const auto time = control.poll();
  playback.tick(time - 950, ff::wall_ms() - 950);
  const auto occurrence = playback.desired()->id;
  playback.committed(occurrence);
  playback.presented(occurrence);
  check(!control.submit([&] {
    std::this_thread::sleep_for(120ms);
    send_request(fd, health);
    check(receive_result(fd).at("selected").at("index") == 0, "playlist clock advances during swap");
  }), "a timer can supersede the frame");
  control.submit([&] {
    std::this_thread::sleep_for(550ms);
    send_request(fd, health);
    pollfd pending{fd, POLLIN, 0};
    check(::poll(&pending, 1, 100) == 0, "a permanently blocked swap loses watchdog liveness");
  });
  server.poll();
  check(receive_result(fd).at("selected").at("index") == 0, "frame loop regained ownership");
  playback.command("artworkPermissions.set", {{"localNetwork",true}});
  int observer = connect_to(path);
  check(setsockopt(observer, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof timeout) == 0, "observer deadline");
  control.submit([&] {
    send_request(observer, hello);
    const auto state = receive_result(observer).at("snapshot");
    send_request(fd, {{"v",1},{"id","policy"},{"method","artworkPermissions.set"},{"params",{{"localNetwork",false}}},
                      {"instance",state.at("instance")},{"generation",state.at("generation")},
                      {"deadlineMs",ff::wall_ms()+5000},{"key","revoke"}});
    send_request(observer, health);
    check(receive_result(observer).at("artworkPermissions").at("localNetwork") == true,
          "grant is not reported revoked while old workers still have it");
    pollfd pending{fd, POLLIN, 0};
    check(::poll(&pending, 1, 50) == 0, "permission receipt waits for the frame owner");
  });
  control.poll();
  receive_result(fd);
  check(playback.global_policy().at("localNetwork") == false, "frame owner applies queued revocation");
  const auto selected = playback.desired()->id;
  const auto generation = playback.generation();
  playback.committed(selected);
  playback.presented(selected);
  control.submit([&] {
    send_request(fd, {{"v",1},{"id","retire"},{"method","playback.update"},
                      {"params",{{"items",ff::Json::array()},{"retireBlockedCurrent",true}}},
                      {"instance",snapshot.at("instance")},{"generation",generation},
                      {"deadlineMs",ff::wall_ms()+5000},{"key","retire-blocked"}});
    send_request(observer, health);
    check(receive_result(observer).at("presented").at("id") == selected, "retirement waits for frame owner");
  });
  control.poll();
  receive_result(fd);
  check(!playback.visible(), "frame owner retires the blocked work");
  playback.set_input_handler({});
  close(observer);
  close(fd);
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
    check_slow_swap(server,playback,path);
    std::filesystem::remove_all(directory);std::cout<<"socket framing, deadlines and slow presentation checks passed\n";
  }catch(const std::exception& error){std::filesystem::remove_all(directory);std::cerr<<error.what()<<'\n';return 1;}
}
