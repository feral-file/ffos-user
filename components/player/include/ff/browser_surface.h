#pragma once
#include <stdint.h>
#ifdef __cplusplus
extern "C" {
#endif
struct ff_browser_server;
struct ff_browser_view;
struct ff_browser_frame {
  int width, height, count, compositor_copies;
  uint32_t format;
  uint64_t modifier;
  int fd[4];
  uint32_t stride[4], offset[4];
};
struct ff_browser_server *ff_browser_server_create(void);
int ff_browser_server_listen(struct ff_browser_server *server, const char *path);
void ff_browser_server_destroy(struct ff_browser_server *server);
void ff_browser_server_poll(struct ff_browser_server *server);
/* Each engine uses its own private socket directory and server. */
struct ff_browser_view *ff_browser_view_create(struct ff_browser_server *server, int width,
                                               int height, int *client_fd);
void ff_browser_view_destroy(struct ff_browser_view *view);
int ff_browser_view_frame(struct ff_browser_view *view, struct ff_browser_frame *frame);
void ff_browser_view_redraw(struct ff_browser_view *view);
void ff_browser_view_frame_requested(struct ff_browser_view *view);
void ff_browser_view_presented_at(struct ff_browser_view *, uint64_t timestamp_ns,
                                  uint32_t refresh_ns, uint32_t flags);
void ff_browser_view_presented(struct ff_browser_view *view);
void ff_browser_view_resize(struct ff_browser_view *view, int width, int height);
void ff_browser_view_pointer(struct ff_browser_view *view, double x, double y);
void ff_browser_view_button(struct ff_browser_view *view, uint32_t button, int down);
void ff_browser_view_wheel(struct ff_browser_view *view, double x, double y);
void ff_browser_view_modifiers(struct ff_browser_view *view, int shift, int control, int alt,
                               int meta);
void ff_browser_view_focus(struct ff_browser_view *view);
void ff_browser_view_blur(struct ff_browser_view *view);
void ff_browser_view_key(struct ff_browser_view *view, uint32_t code, int down);
#ifdef __cplusplus
}
#endif
