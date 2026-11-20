#define WLR_USE_UNSTABLE
#include "ff/browser_surface.h"

#include <linux/input-event-codes.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>
#include <wayland-server-core.h>
#include <wlr/backend.h>
#include <wlr/backend/headless.h>
#include <wlr/interfaces/wlr_keyboard.h>
#include <wlr/interfaces/wlr_output.h>
#include <wlr/render/allocator.h>
#include <wlr/render/dmabuf.h>
#include <wlr/render/wlr_renderer.h>
#include <wlr/types/wlr_buffer.h>
#include <wlr/types/wlr_compositor.h>
#include <wlr/types/wlr_data_device.h>
#include <wlr/types/wlr_keyboard.h>
#include <wlr/types/wlr_linux_dmabuf_v1.h>
#include <wlr/types/wlr_output_layout.h>
#include <wlr/types/wlr_presentation_time.h>
#include <wlr/types/wlr_scene.h>
#include <wlr/types/wlr_seat.h>
#include <wlr/types/wlr_single_pixel_buffer_v1.h>
#include <wlr/types/wlr_subcompositor.h>
#include <wlr/types/wlr_viewporter.h>
#include <wlr/types/wlr_xdg_output_v1.h>
#include <wlr/types/wlr_xdg_shell.h>
#include <wlr/util/box.h>
#include <xkbcommon/xkbcommon.h>

struct ff_browser_server {
  struct wl_display *display;
  struct wlr_backend *backend;
  struct wlr_renderer *renderer;
  struct wlr_allocator *allocator;
  struct wlr_linux_dmabuf_v1 *dmabuf;
  struct wlr_seat *seat;
  struct wlr_keyboard keyboard;
  struct wl_listener toplevel, popup, key, modifiers;
  struct wl_list views;
  int shell_ready, keyboard_ready;
};
struct ff_browser_view {
  struct wl_list link;
  struct ff_browser_server *server;
  struct wl_client *client;
  struct wlr_xdg_toplevel *toplevel;
  struct wlr_output *output;
  struct wlr_scene *scene;
  struct wlr_scene_output *scene_output;
  struct wlr_output_state frame_state;
  int frame_pending;
  int compositor_copies;
  uint32_t pending_present_seq;
  struct wl_listener commit, destroy, client_destroy;
  int width, height, dirty;
};
static uint32_t milliseconds(void) {
  struct timespec now;
  clock_gettime(CLOCK_MONOTONIC, &now);
  return (uint32_t)((uint64_t)now.tv_sec * 1000 + now.tv_nsec / 1000000);
}
static void on_commit(struct wl_listener *listener, void *data) {
  (void)data;
  struct ff_browser_view *view = wl_container_of(listener, view, commit);
  if (view->toplevel->base->initial_commit) {
    wlr_xdg_toplevel_set_size(view->toplevel, view->width, view->height);
    wlr_xdg_toplevel_set_fullscreen(view->toplevel, true);
    wlr_xdg_toplevel_set_activated(view->toplevel, true);
  }
  view->dirty = 1;
}
static void on_destroy(struct wl_listener *listener, void *data) {
  (void)data;
  struct ff_browser_view *view = wl_container_of(listener, view, destroy);
  wl_list_remove(&view->commit.link);
  wl_list_remove(&view->destroy.link);
  view->toplevel = NULL;
  view->dirty = 0;
}
static void on_client_destroy(struct wl_listener *listener, void *data) {
  (void)data;
  struct ff_browser_view *v = wl_container_of(listener, v, client_destroy);
  wl_list_remove(&v->client_destroy.link);
  v->client = NULL;
}
static void on_toplevel(struct wl_listener *listener, void *data) {
  struct ff_browser_server *server = wl_container_of(listener, server, toplevel);
  struct wlr_xdg_toplevel *top = data;
  struct ff_browser_view *view;
  struct wl_client *client = wl_resource_get_client(top->base->surface->resource);
  wl_list_for_each(view, &server->views, link) {
    if (view->client && view->client != client) continue;
    if (!view->client) {
      view->client = client;
      view->client_destroy.notify = on_client_destroy;
      wl_client_add_destroy_listener(client, &view->client_destroy);
    }
    /* Artwork cannot create another device window or cover host-owned UI. */
    if (view->toplevel) {
      wlr_xdg_toplevel_send_close(top);
      return;
    }
    view->toplevel = top;
    top->base->data = wlr_scene_xdg_surface_create(&view->scene->tree, top->base);
    view->commit.notify = on_commit;
    wl_signal_add(&top->base->surface->events.commit, &view->commit);
    view->destroy.notify = on_destroy;
    wl_signal_add(&top->events.destroy, &view->destroy);
    return;
  }
  wlr_xdg_toplevel_send_close(top);
}
struct browser_popup {
  struct wlr_xdg_popup *popup;
  struct wl_listener commit, destroy;
};
static void on_popup_commit(struct wl_listener *listener, void *data) {
  (void)data;
  struct browser_popup *state = wl_container_of(listener, state, commit);
  if (state->popup->base->initial_commit) wlr_xdg_surface_schedule_configure(state->popup->base);
}
static void on_popup_destroy(struct wl_listener *listener, void *data) {
  (void)data;
  struct browser_popup *state = wl_container_of(listener, state, destroy);
  wl_list_remove(&state->commit.link);
  wl_list_remove(&state->destroy.link);
  free(state);
}
static void on_popup(struct wl_listener *listener, void *data) {
  struct ff_browser_server *server = wl_container_of(listener, server, popup);
  struct wlr_xdg_popup *popup = data;
  struct wlr_xdg_surface *parent = wlr_xdg_surface_try_from_wlr_surface(popup->parent);
  struct ff_browser_view *view;
  wl_list_for_each(view, &server->views, link) {
    if (!view->toplevel || !parent || !parent->data ||
        wl_resource_get_client(popup->base->surface->resource) != view->client)
      continue;
    struct browser_popup *state = calloc(1, sizeof(*state));
    if (!state) break;
    state->popup = popup;
    // Browser widget menus remain clipped to this occurrence's exported output.
    // New artist page targets are separately denied by the engine adapter.
    popup->base->data = wlr_scene_xdg_surface_create(parent->data, popup->base);
    struct wlr_box box = {.width = view->width, .height = view->height};
    wlr_xdg_popup_unconstrain_from_box(popup, &box);
    state->commit.notify = on_popup_commit;
    wl_signal_add(&popup->base->surface->events.commit, &state->commit);
    state->destroy.notify = on_popup_destroy;
    wl_signal_add(&popup->events.destroy, &state->destroy);
    return;
  }
  wlr_xdg_popup_destroy(popup);
}
static void on_key(struct wl_listener *listener, void *data) {
  struct ff_browser_server *s = wl_container_of(listener, s, key);
  struct wlr_keyboard_key_event *e = data;
  wlr_seat_keyboard_notify_key(s->seat, e->time_msec, e->keycode, e->state);
}
static void on_modifiers(struct wl_listener *listener, void *data) {
  (void)data;
  struct ff_browser_server *s = wl_container_of(listener, s, modifiers);
  wlr_seat_keyboard_notify_modifiers(s->seat, &s->keyboard.modifiers);
}
struct ff_browser_server *ff_browser_server_create(void) {
  struct ff_browser_server *s = calloc(1, sizeof(*s));
  if (!s) return NULL;
  wl_list_init(&s->views);
  s->display = wl_display_create();
  if (!s->display) goto fail;
  s->backend = wlr_headless_backend_create(wl_display_get_event_loop(s->display));
  if (!s->backend) goto fail;
  s->renderer = wlr_renderer_autocreate(s->backend);
  if (!s->renderer) goto fail;
  s->allocator = wlr_allocator_autocreate(s->backend, s->renderer);
  if (!s->allocator) goto fail;
  if (!wlr_renderer_init_wl_shm(s->renderer, s->display)) goto fail;
  wlr_compositor_create(s->display, 6, s->renderer);
  wlr_subcompositor_create(s->display);
  s->dmabuf = wlr_linux_dmabuf_v1_create_with_renderer(s->display, 4, s->renderer);
  wlr_presentation_create(s->display, s->backend, 2);
  wlr_single_pixel_buffer_manager_v1_create(s->display);
  wlr_viewporter_create(s->display);
  wlr_data_device_manager_create(s->display);
  struct wlr_xdg_shell *shell = wlr_xdg_shell_create(s->display, 6);
  s->toplevel.notify = on_toplevel;
  wl_signal_add(&shell->events.new_toplevel, &s->toplevel);
  s->popup.notify = on_popup;
  wl_signal_add(&shell->events.new_popup, &s->popup);
  s->shell_ready = 1;
  s->seat = wlr_seat_create(s->display, "ff-player");
  if (!s->seat) goto fail;
  static const struct wlr_keyboard_impl impl = {.name = "ff-player"};
  wlr_keyboard_init(&s->keyboard, &impl, "ff-player");
  struct xkb_context *context = xkb_context_new(XKB_CONTEXT_NO_FLAGS);
  if (!context) {
    wlr_keyboard_finish(&s->keyboard);
    goto fail;
  }
  struct xkb_keymap *map = xkb_keymap_new_from_names(context, NULL, XKB_KEYMAP_COMPILE_NO_FLAGS);
  if (!map) {
    xkb_context_unref(context);
    wlr_keyboard_finish(&s->keyboard);
    goto fail;
  }
  wlr_keyboard_set_keymap(&s->keyboard, map);
  xkb_keymap_unref(map);
  xkb_context_unref(context);
  wlr_keyboard_set_repeat_info(&s->keyboard, 25, 600);
  wlr_seat_set_keyboard(s->seat, &s->keyboard);
  s->key.notify = on_key;
  wl_signal_add(&s->keyboard.events.key, &s->key);
  s->modifiers.notify = on_modifiers;
  wl_signal_add(&s->keyboard.events.modifiers, &s->modifiers);
  s->keyboard_ready = 1;
  wlr_seat_set_capabilities(s->seat, WL_SEAT_CAPABILITY_POINTER | WL_SEAT_CAPABILITY_KEYBOARD);
  if (!wlr_backend_start(s->backend)) goto fail;
  return s;
fail:
  ff_browser_server_destroy(s);
  return NULL;
}
int ff_browser_server_listen(struct ff_browser_server *s, const char *path) {
  return wl_display_add_socket(s->display, path);
}
void ff_browser_server_poll(struct ff_browser_server *s) {
  wl_event_loop_dispatch(wl_display_get_event_loop(s->display), 0);
  wl_display_flush_clients(s->display);
}
// The upstream headless output reports presentation immediately at commit.
// That is premature for a nested host and makes Chromium queue GPU work ahead
// of native composition. This output acknowledges only after host submission.
static bool output_test(struct wlr_output *output, const struct wlr_output_state *state) {
  (void)output;
  return !(state->committed & ~(WLR_OUTPUT_STATE_BACKEND_OPTIONAL | WLR_OUTPUT_STATE_BUFFER |
                                WLR_OUTPUT_STATE_ENABLED | WLR_OUTPUT_STATE_MODE));
}
static bool output_commit(struct wlr_output *output, const struct wlr_output_state *state) {
  if (!output_test(output, state)) return false;
  struct ff_browser_view *view = output->data;
  if (state->committed & WLR_OUTPUT_STATE_BUFFER)
    view->pending_present_seq = output->commit_seq + 1;
  return true;
}
static void output_destroy(struct wlr_output *output) {
  wlr_output_finish(output);
  free(output);
}
static const struct wlr_output_impl output_impl = {
    .destroy = output_destroy,
    .test = output_test,
    .commit = output_commit,
};
struct ff_browser_view *ff_browser_view_create(struct ff_browser_server *s, int width, int height,
                                               int *fd) {
  int pair[2] = {-1, -1};
  if (fd && socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, pair) < 0) return NULL;
  struct ff_browser_view *v = calloc(1, sizeof(*v));
  if (!v) {
    close(pair[0]);
    close(pair[1]);
    return NULL;
  }
  v->server = s;
  v->width = width;
  v->height = height;
  v->client = fd ? wl_client_create(s->display, pair[0]) : NULL;
  if (fd && !v->client) {
    close(pair[0]);
    close(pair[1]);
    free(v);
    return NULL;
  }
  if (v->client) {
    v->client_destroy.notify = on_client_destroy;
    wl_client_add_destroy_listener(v->client, &v->client_destroy);
  }
  v->output = calloc(1, sizeof(*v->output));
  if (v->output) {
    struct wlr_output_state initial;
    wlr_output_state_init(&initial);
    wlr_output_state_set_custom_mode(&initial, width, height, 60000);
    wlr_output_init(v->output, s->backend, &output_impl, wl_display_get_event_loop(s->display),
                    &initial);
    wlr_output_state_finish(&initial);
    v->output->data = v;
    wlr_output_set_name(v->output, "FF-ARTWORK");
  }
  if (!v->output) {
    if (v->client) wl_client_destroy(v->client);
    close(pair[1]);
    free(v);
    return NULL;
  }
  wlr_output_init_render(v->output, s->allocator, s->renderer);
  struct wlr_output_state state;
  wlr_output_state_init(&state);
  wlr_output_state_set_enabled(&state, true);
  wlr_output_state_set_custom_mode(&state, width, height, 60000);
  bool ok = wlr_output_commit_state(v->output, &state);
  wlr_output_state_finish(&state);
  if (!ok) {
    wlr_output_destroy(v->output);
    if (v->client) wl_client_destroy(v->client);
    close(pair[1]);
    free(v);
    return NULL;
  }
  v->scene = wlr_scene_create();
  wlr_scene_set_linux_dmabuf_v1(v->scene, s->dmabuf);
  v->scene_output = wlr_scene_output_create(v->scene, v->output);
  wlr_output_create_global(v->output, s->display);
  wl_list_insert(&s->views, &v->link);
  if (fd) *fd = pair[1];
  return v;
}
void ff_browser_view_destroy(struct ff_browser_view *v) {
  if (!v) return;
  if (v->toplevel) {
    wl_list_remove(&v->commit.link);
    wl_list_remove(&v->destroy.link);
  }
  if (v->client) {
    wl_list_remove(&v->client_destroy.link);
    wl_client_destroy(v->client);
  }
  if (v->frame_pending) wlr_output_state_finish(&v->frame_state);
  wlr_scene_node_destroy(&v->scene->tree.node);
  wlr_output_destroy(v->output);
  wl_list_remove(&v->link);
  free(v);
}
void ff_browser_server_destroy(struct ff_browser_server *s) {
  if (!s) return;
  struct ff_browser_view *v, *tmp;
  wl_list_for_each_safe(v, tmp, &s->views, link) ff_browser_view_destroy(v);
  if (s->display) wl_display_destroy_clients(s->display);
  if (s->shell_ready) {
    wl_list_remove(&s->toplevel.link);
    wl_list_remove(&s->popup.link);
  }
  if (s->keyboard_ready) {
    wl_list_remove(&s->key.link);
    wl_list_remove(&s->modifiers.link);
    wlr_seat_set_keyboard(s->seat, NULL);
    wlr_keyboard_finish(&s->keyboard);
  }
  if (s->allocator) wlr_allocator_destroy(s->allocator);
  if (s->renderer) wlr_renderer_destroy(s->renderer);
  if (s->backend) wlr_backend_destroy(s->backend);
  if (s->display) wl_display_destroy(s->display);
  free(s);
}
struct frame_observer {
  struct wl_listener listener;
  struct ff_browser_view *view;
};
struct frame_observers {
  struct frame_observer entries[64];
  int count;
  struct ff_browser_view *view;
};
static void sampled(struct wl_listener *listener, void *data) {
  struct frame_observer *observer = wl_container_of(listener, observer, listener);
  struct wlr_scene_output_sample_event *event = data;
  observer->view->compositor_copies = event->direct_scanout ? 0 : 1;
}
static void observe_buffer(struct wlr_scene_buffer *buffer, int x, int y, void *data) {
  (void)x;
  (void)y;
  struct frame_observers *observers = data;
  if (observers->count >= 64) return;
  struct frame_observer *observer = &observers->entries[observers->count++];
  observer->view = observers->view;
  observer->listener.notify = sampled;
  wl_signal_add(&buffer->events.output_sample, &observer->listener);
}
void ff_browser_view_redraw(struct ff_browser_view *v) {
  /* A load/ready event need not change pixels. Re-export the retained scene
   * once so presentation can commit a static document without waiting for an
   * artist animation or injecting a repaint into its document. */
  wlr_damage_ring_add_whole(&v->scene_output->damage_ring);
  wlr_output_schedule_frame(v->output);
}
int ff_browser_view_frame(struct ff_browser_view *v, struct ff_browser_frame *frame) {
  if (!v->toplevel || !wlr_scene_output_needs_frame(v->scene_output)) return 0;
  if (v->frame_pending) {
    wlr_output_state_finish(&v->frame_state);
    v->frame_pending = 0;
  }
  wlr_output_state_init(&v->frame_state);
  v->frame_pending = 1;
  struct frame_observers observers = {.view = v};
  v->compositor_copies = 1;
  wlr_scene_node_for_each_buffer(&v->scene->tree.node, observe_buffer, &observers);
  bool built = wlr_scene_output_build_state(v->scene_output, &v->frame_state, NULL);
  for (int i = 0; i < observers.count; i++) wl_list_remove(&observers.entries[i].listener.link);
  if (!built) return -1;
  frame->compositor_copies = v->compositor_copies;
  if (!v->frame_state.buffer) return 0;
  struct wlr_dmabuf_attributes attrs;
  if (!wlr_buffer_get_dmabuf(v->frame_state.buffer, &attrs)) return -1;
  if (!wlr_output_commit_state(v->output, &v->frame_state)) return -1;
  frame->width = attrs.width;
  frame->height = attrs.height;
  frame->format = attrs.format;
  frame->modifier = attrs.modifier;
  frame->count = attrs.n_planes;
  for (int i = 0; i < attrs.n_planes; i++) {
    frame->fd[i] = attrs.fd[i];
    frame->stride[i] = attrs.stride[i];
    frame->offset[i] = attrs.offset[i];
  }
  v->dirty = 0;
  return 1;
}
void *ff_browser_view_lock_frame(struct ff_browser_view *v) {
  return wlr_buffer_lock(v->frame_state.buffer);
}
void ff_browser_frame_unlock(void *buffer) {
  wlr_buffer_unlock(buffer);
}
void ff_browser_view_presented_at(struct ff_browser_view *v, uint64_t timestamp_ns,
                                  uint32_t refresh_ns, uint32_t flags) {
  if (v->toplevel) {
    struct timespec now = {.tv_sec = timestamp_ns / 1000000000,
                           .tv_nsec = timestamp_ns % 1000000000};
    if (v->pending_present_seq) {
      struct wlr_output_event_present present = {
          .commit_seq = v->pending_present_seq,
          .presented = true,
          .when = now,
          .refresh = refresh_ns,
          .flags = flags & ~WLR_OUTPUT_PRESENT_ZERO_COPY,
      };
      wlr_output_send_present(v->output, &present);
      v->pending_present_seq = 0;
    }
  }
}
void ff_browser_view_frame_requested(struct ff_browser_view *v) {
  if (!v->toplevel) return;
  struct timespec now;
  clock_gettime(CLOCK_MONOTONIC, &now);
  /* frame_done permits the client to render ahead; presentation feedback
   * independently describes when the sampled buffer reached Cage. The output
   * state keeps that source locked until the asynchronous host copy completes.
   * Waiting for scanout here serializes two GPU renderers and causes bursts. */
  wlr_scene_output_send_frame_done(v->scene_output, &now);
  wlr_output_send_frame(v->output);
}
void ff_browser_view_presented(struct ff_browser_view *v) {
  struct timespec now;
  clock_gettime(CLOCK_MONOTONIC, &now);
  // Startup/feedback fallback has no scanout claim. Normal acknowledged frames
  // carry Cage's actual presentation timestamp and flags, excluding ZERO_COPY.
  ff_browser_view_presented_at(v, (uint64_t)now.tv_sec * 1000000000 + now.tv_nsec, 16666666, 0);
}
void ff_browser_view_resize(struct ff_browser_view *v, int width, int height) {
  if (width == v->width && height == v->height) return;
  v->width = width;
  v->height = height;
  struct wlr_output_state state;
  wlr_output_state_init(&state);
  wlr_output_state_set_custom_mode(&state, width, height, 60000);
  wlr_output_commit_state(v->output, &state);
  wlr_output_state_finish(&state);
  if (v->toplevel) wlr_xdg_toplevel_set_size(v->toplevel, width, height);
}
static struct wlr_surface *focus(struct ff_browser_view *v) {
  if (!v->toplevel) return NULL;
  struct wlr_surface *surface = v->toplevel->base->surface;
  if (v->server->seat->keyboard_state.focused_surface != surface)
    wlr_seat_keyboard_notify_enter(v->server->seat, surface, v->server->keyboard.keycodes,
                                   v->server->keyboard.num_keycodes,
                                   &v->server->keyboard.modifiers);
  return surface;
}
struct pointer_offset {
  struct wlr_surface *target;
  int x, y;
};
static void pointer_surface_offset(struct wlr_surface *surface, int x, int y, void *data) {
  struct pointer_offset *offset = data;
  if (surface == offset->target) {
    offset->x = x;
    offset->y = y;
  }
}
void ff_browser_view_pointer(struct ff_browser_view *v, double x, double y) {
  if (!focus(v)) return;
  double sx = x, sy = y;
  struct wlr_surface *surface = wlr_xdg_surface_surface_at(v->toplevel->base, x, y, &sx, &sy);
  if (!surface) {
    if (v->server->seat->pointer_state.button_count == 0)
      wlr_seat_pointer_notify_clear_focus(v->server->seat);
    else {
      struct pointer_offset offset = {.target = v->server->seat->pointer_state.focused_surface};
      wlr_surface_for_each_surface(v->toplevel->base->surface, pointer_surface_offset, &offset);
      wlr_seat_pointer_notify_motion(v->server->seat, milliseconds(), x - offset.x, y - offset.y);
      wlr_seat_pointer_notify_frame(v->server->seat);
    }
    return;
  }
  wlr_seat_pointer_notify_enter(v->server->seat, surface, sx, sy);
  wlr_seat_pointer_notify_motion(v->server->seat, milliseconds(), sx, sy);
  wlr_seat_pointer_notify_frame(v->server->seat);
}
void ff_browser_view_button(struct ff_browser_view *v, uint32_t button, int down) {
  if (!focus(v)) return;
  wlr_seat_pointer_notify_button(
      v->server->seat, milliseconds(), button,
      down ? WL_POINTER_BUTTON_STATE_PRESSED : WL_POINTER_BUTTON_STATE_RELEASED);
  wlr_seat_pointer_notify_frame(v->server->seat);
}
void ff_browser_view_wheel(struct ff_browser_view *v, double x, double y) {
  if (!focus(v)) return;
  wlr_seat_pointer_notify_axis(v->server->seat, milliseconds(), WL_POINTER_AXIS_HORIZONTAL_SCROLL,
                               x, 0, WL_POINTER_AXIS_SOURCE_WHEEL,
                               WL_POINTER_AXIS_RELATIVE_DIRECTION_IDENTICAL);
  wlr_seat_pointer_notify_axis(v->server->seat, milliseconds(), WL_POINTER_AXIS_VERTICAL_SCROLL, y,
                               0, WL_POINTER_AXIS_SOURCE_WHEEL,
                               WL_POINTER_AXIS_RELATIVE_DIRECTION_IDENTICAL);
  wlr_seat_pointer_notify_frame(v->server->seat);
}
void ff_browser_view_key(struct ff_browser_view *v, uint32_t code, int down) {
  if (!focus(v)) return;
  struct wlr_keyboard_key_event e = {
      .time_msec = milliseconds(),
      .keycode = code,
      .update_state = true,
      .state = down ? WL_KEYBOARD_KEY_STATE_PRESSED : WL_KEYBOARD_KEY_STATE_RELEASED};
  wlr_keyboard_notify_key(&v->server->keyboard, &e);
}

void ff_browser_view_blur(struct ff_browser_view *v) {
  wlr_seat_pointer_notify_clear_focus(v->server->seat);
  wlr_seat_keyboard_notify_clear_focus(v->server->seat);
}

void ff_browser_view_focus(struct ff_browser_view *v) { focus(v); }

void ff_browser_view_modifiers(struct ff_browser_view *v, int shift, int control, int alt,
                               int meta) {
  struct wlr_keyboard *keyboard = &v->server->keyboard;
  uint32_t mask = 0;
  const char *names[] = {XKB_MOD_NAME_SHIFT, XKB_MOD_NAME_CTRL, XKB_MOD_NAME_ALT,
                         XKB_MOD_NAME_LOGO};
  int active[] = {shift, control, alt, meta};
  for (int i = 0; i < 4; i++) {
    xkb_mod_index_t index = xkb_keymap_mod_get_index(keyboard->keymap, names[i]);
    if (active[i] && index != XKB_MOD_INVALID && index < 32) mask |= 1U << index;
  }
  wlr_keyboard_notify_modifiers(keyboard, mask, 0, 0, 0);
}
