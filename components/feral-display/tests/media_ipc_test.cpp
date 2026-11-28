#include "ff/media_ipc.hpp"
#include <sys/socket.h>
#include <fcntl.h>
#include <unistd.h>
#include <iostream>
int main(){
  try{
    int sockets[2];ff::require(socketpair(AF_UNIX,SOCK_SEQPACKET|SOCK_CLOEXEC,0,sockets)==0,"test","socketpair");
    ff::require(!ff::receive_media(sockets[1]),"test","Empty nonblocking channel");
    int file[2];ff::require(pipe(file)==0,"test","pipe");ff::require(write(file[1],"pixels stay on GPU",18)==18,"test","write");close(file[1]);
    ff::send_media(sockets[0],{{"event","frame"},{"sequence",1}},{file[0]});close(file[0]);
    auto packet=ff::receive_media(sockets[1]);ff::require(packet&&packet->body["sequence"]==1&&packet->descriptors.size()==1,"test","Descriptor packet");
    char bytes[18];ff::require(read(packet->descriptors[0],bytes,18)==18,"test","Transferred FD survives sender close");
    ff::require(fcntl(packet->descriptors[0],F_GETFD)&FD_CLOEXEC,"test","Received FD must not leak to another worker");
    auto descriptor=packet->descriptors[0];packet.reset();ff::require(fcntl(descriptor,F_GETFD)==-1,"test","Packet destruction closes descriptors");
    bool rejected=false;try{ff::send_media(sockets[0],{{"large",std::string(ff::media_packet_limit,'x')}});}catch(const ff::Error&){rejected=true;}
    ff::require(rejected,"test","Oversize packet rejected before send");
    close(sockets[0]);rejected=false;try{ff::receive_media(sockets[1]);}catch(const ff::Error&){rejected=true;}
    ff::require(rejected,"test","Worker exit must not look like no new frame");close(sockets[1]);
    std::cout<<"media IPC descriptor, bounds, lifetime and disconnect checks passed\n";
  }catch(const std::exception& error){std::cerr<<error.what()<<'\n';return 1;}
}
