package commands

import "context"

type controllerCtxKey struct{}

// FromController marks ctx as carrying commands sent by a controller outside
// the device: the app over the LAN hub or the relayer, a CLI, an agent. The
// router reads the mark to tell a person's cast from the device's own.
// Commands built in-process (playlistschedule cutovers, OOM-recovery re-casts)
// never carry it.
func FromController(ctx context.Context) context.Context {
	return context.WithValue(ctx, controllerCtxKey{}, true)
}

// IsFromController reports whether ctx was marked by FromController.
func IsFromController(ctx context.Context) bool {
	v, _ := ctx.Value(controllerCtxKey{}).(bool)
	return v
}
