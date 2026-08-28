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
	// Sources inside are log-truncated AND log-only (see
	// SourceProbeResult's sanitization rule): Error() below must never
	// render them, because it is returned verbatim to casters.
	Results []offlinecache.SourceProbeResult
}

// maxProbeLogDetailItems caps how many dead items the displayPlaylist
// preflight logs individually (with their truncated sources) before the
// remainder collapses into a count — same amplification concern as
// maxErrorDetailItems below, aimed at the log instead of the response.
const maxProbeLogDetailItems = 5

// maxErrorDetailItems caps how many per-item entries Error() renders.
// The unauthenticated hub accepts a 4 MiB playlist with no item cap, so
// an uncapped join would let one hostile all-dead cast mint an error
// response proportional to its own size; past the cap a single
// omitted-count entry stands in for the rest.
const maxErrorDetailItems = 10

// Error renders items by INDEX and STATUS only — never the source URL.
// Resolved item sources are playlist content a playlistUrl or
// dynamic-playlist caller never supplied, and signed CDN URLs carry
// credentials in their query strings; the controller contract requires
// sanitized error messages, and both ingress paths return this string
// verbatim. The caster holds the playlist, so an index is enough to
// name the item. Probe error details (r.Err) are excluded for the same
// reason: they can quote what they were probing.
func (e *SourceUnreachableError) Error() string {
	parts := make([]string, 0, min(len(e.Results), maxErrorDetailItems+1))
	for i, r := range e.Results {
		if i == maxErrorDetailItems {
			parts = append(parts, fmt.Sprintf("and %d more", len(e.Results)-i))
			break
		}
		if r.Status != 0 {
			parts = append(parts, fmt.Sprintf("item %d: HTTP %d", i, r.Status))
		} else {
			// A dead item with no status is the malformed-data:-URI case
			// (the only non-HTTP dead verdict).
			parts = append(parts, fmt.Sprintf("item %d: unusable data: URI", i))
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
