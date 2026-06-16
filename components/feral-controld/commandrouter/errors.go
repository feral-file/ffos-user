package commandrouter

import (
	"errors"
	"fmt"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
)

// RateLimitedError is returned by the command storm gate when a command is
// rejected to protect the device from flooding. Callers (the LAN hub and the
// relayer mediator) detect it to report a legible failure instead of treating
// it as an internal error.
type RateLimitedError struct {
	// Command is the command type that was rejected.
	Command commands.Type
	// Reason describes which guard rejected the command (rate, concurrency).
	Reason string
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("command %q rejected by storm protection: %s", e.Command, e.Reason)
}

// IsRateLimited reports whether err is (or wraps) a RateLimitedError.
func IsRateLimited(err error) bool {
	var rle *RateLimitedError
	return errors.As(err, &rle)
}
