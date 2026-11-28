#define _GNU_SOURCE
#include "ff/browser_surface.h"
#include <stdio.h>
#include <stdlib.h>
#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>
int main(int argc,char **argv){
 if(argc!=3)return 2;
 char directory[]="/tmp/ff-browser-probe-XXXXXX";if(!mkdtemp(directory))return 3;
 char socket[108],profile[128];snprintf(socket,sizeof(socket),"%s/wayland",directory);snprintf(profile,sizeof(profile),"--user-data-dir=%s/profile",directory);
 struct ff_browser_server *server=ff_browser_server_create();if(!server)return 3;
 if(ff_browser_server_listen(server,socket)!=0)return 4;
 struct ff_browser_view *view=ff_browser_view_create(server,1280,720,NULL);if(!view)return 4;
 pid_t pid=fork();if(pid==0){unsetenv("WAYLAND_SOCKET");setenv("WAYLAND_DISPLAY",socket,1);execl(argv[1],argv[1],"--ozone-platform=wayland","--kiosk","--no-first-run",profile,"--test-type=gpu","--enable-unsafe-webgpu","--use-angle=vulkan","--enable-features=Vulkan,VulkanFromANGLE","--disable-vulkan-surface",argv[2],NULL);_exit(127);}
 if(pid<0)return 5;
 int frames=0;
 for(int i=0;i<1200;i++){
  ff_browser_server_poll(server);struct ff_browser_frame frame;int result=ff_browser_view_frame(view,&frame);
  if(result>0){if(!frames)printf("DMA-BUF %dx%d format=%u planes=%d\n",frame.width,frame.height,frame.format,frame.count);frames++;}
  if(result<0){fprintf(stderr,"CPU buffer unsupported\n");break;}
  ff_browser_view_presented(view);usleep(16667);
 }
 kill(pid,SIGTERM);int status=0;for(int i=0;i<100;i++){if(waitpid(pid,&status,WNOHANG)==pid){pid=-1;break;}usleep(10000);}if(pid>0){kill(pid,SIGKILL);waitpid(pid,NULL,0);}
 ff_browser_view_destroy(view);ff_browser_server_destroy(server);printf("frames=%d (surface observations; no physical-display claim)\n",frames);return frames?0:5;
}
