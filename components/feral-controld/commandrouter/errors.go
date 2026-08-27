package commandrouter

import (
	"errors"
	"fmt"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
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

// SourceUnreachableError is returned by the displayPlaylist path when the
// cast-time source preflight got a DEFINITIVE dead verdict for every item
// (see the probe call in handler.go for the fail-open rules that make
// "every" the only rejectable shape). The name deliberately matches DP-1
// core spec §14's player error code for this scenario, `sourceUnreachable`,
// so the string a controller surfaces to its user maps onto the spec's
// vocabulary. Callers (the LAN hub, the relayer mediator) detect it to
// report an actionable client-side failure instead of an internal error.
type SourceUnreachableError struct {
	// Results holds every item's probe outcome, in playlist order.
	// Sources inside are already log-truncated (see SourceProbeResult).
	Results []offlinecache.SourceProbeResult
}

func (e *SourceUnreachableError) Error() string {
	parts := make([]string, 0, len(e.Results))
	for _, r := range e.Results {
		switch {
		case r.Status != 0:
			parts = append(parts, fmt.Sprintf("HTTP %d: %s", r.Status, r.Source))
		case r.Err != nil:
			parts = append(parts, fmt.Sprintf("%v: %s", r.Err, r.Source))
		}
	}
	return fmt.Sprintf("sourceUnreachable: no playlist item source is loadable (%s)",
		strings.Join(parts, "; "))
}

// IsSourceUnreachable reports whether err is (or wraps) a
// SourceUnreachableError.
func IsSourceUnreachable(err error) bool {
	var sue *SourceUnreachableError
	return errors.As(err, &sue)
}
