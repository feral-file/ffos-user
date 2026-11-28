package playerlink

import (
	"os"
	"path/filepath"
	"strconv"
)

// SocketPath may be overridden for an isolated development installation. There
// is deliberately no CDP fallback: the image must install matching components.
func SocketPath() string {
	if path := os.Getenv("FF_PLAYER_SOCKET"); path != "" {
		return path
	}
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	return filepath.Join(runtime, "ff-player/player.sock")
}
