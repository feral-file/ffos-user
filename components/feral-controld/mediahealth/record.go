// Package mediahealth is feral-controld's device-side media observation
// store (feral-file/ffos-user#392): one record per playlist item source,
// written by the L0 byte probe (this package), by the play outcomes the
// player reports, and later by the L1 nightly render. The record decides
// two things and reports a third:
//
//   - the per-item HINT handed to the player on every displayPlaylist
//     push, so a conclusively broken item is skipped without being mounted;
//   - the admission verdict of a cast (every item dead ⇒ sourceUnreachable),
//     derived from the same probe pass that used to be the standalone
//     preflight (#304);
//   - the per-item health the app sees on player_status / getMediaHealth.
//
// Boundary rules (pinned by importlint_test.go): this package never imports
// the transports (hub, relayer, mediator) or the router (commandrouter). It
// never dials on its own either — every network fetch goes through the
// Fetcher the offline cache builds over its source guard, because dialing
// playlist-supplied URLs is only safe there (see offlinecache.ErrUnsafeSource).
//
// Two invariants every writer must keep:
//
//  1. No observation hides a work. A hint is produced only by this device's
//     own CONCLUSIVE verdict (a broken record inside its recheck window); a
//     transient failure (429, 5xx, timeout, guard refusal) is never persisted
//     as a verdict. Nothing central is ever consumed as a block list.
//  2. Source URLs stay on the device. The record holds the source so hints
//     can be keyed by it over loopback CDP; every log line goes through the
//     Fetcher's redacted label, and the status surfaces (ItemHealth, Report)
//     carry no URL at all.
package mediahealth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"time"
)

// Key is the store key for a source: sha256 hex over the exact source
// string. It is byte-identical to offlinecache.SourceKey (the cache identity)
// on purpose — the same item is the same record in both stores — and pinned
// equal by a test in that package, since this one cannot import it.
func Key(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

// State is a record's verdict. The absence of a record is "unknown"; the
// surfaces spell that out (StateUnknown) rather than inventing a record.
type State string

const (
	// StateOK: the last conclusive observation rendered or passed L0.
	StateOK State = "ok"
	// StateBroken: a conclusive failure. Hints and the admission verdict
	// read it only while the record is inside its recheck window
	// (Record.Conclusive); the status surfaces show it regardless, since it
	// is still the last thing this device saw.
	StateBroken State = "broken"
	// StateSuspect: an L1 (nightly render) verdict. Reported to the app,
	// never hinted and never counted toward rejection — L1 is an experiment
	// (its gate is behind config) and a blank frame is weaker evidence than
	// a byte-level failure.
	StateSuspect State = "suspect"
	// StateUnknown is a surface value only: no record exists, or the record
	// carries no verdict yet (transient attempts only). Never stored.
	StateUnknown State = "unknown"
)

// Layer names which probe set the current verdict.
type Layer string

const (
	LayerL0   Layer = "l0"
	LayerPlay Layer = "play"
	LayerL1   Layer = "l1"
)

// Category is the app-facing failure class: a closed set the mobile app
// renders a label for. Reason carries the detail code underneath it.
type Category string

const (
	// CategorySourceMissing: the origin said the resource is not there for
	// anyone (404/410-class), or the host does not resolve.
	CategorySourceMissing Category = "source_missing"
	// CategorySourceBlocked: the kiosk itself was refused (401/403 seen by
	// the player's own HEAD). L0 treats those statuses as transient because
	// its request identity is not the kiosk's; only the play layer can
	// conclude them.
	CategorySourceBlocked Category = "source_blocked"
	// CategoryNotMedia: the bytes are a page, not the media the item
	// declares — an error page, a gateway directory listing, a JSON error.
	CategoryNotMedia Category = "not_media"
	// CategoryCorruptFile: empty, truncated, or a container whose header
	// does not parse.
	CategoryCorruptFile Category = "corrupt_file"
	// CategoryUnsupported: a shape the device cannot play at all (an
	// unsupported scheme, a malformed data: URI).
	CategoryUnsupported Category = "unsupported"
	// CategoryRenderFailed: the player mounted it and it failed (load,
	// decode, watchdog, exhausted retries). Corroborated before it counts.
	CategoryRenderFailed Category = "render_failed"
	// CategoryRenderBlank: the L1 nightly render saw a blank frame or a
	// known-bad fingerprint. Always StateSuspect.
	CategoryRenderBlank Category = "render_blank"
	// CategoryDeviceLimit: a device-local condition (WebGL context loss).
	// Recorded on the play stats, never a verdict about the source.
	CategoryDeviceLimit Category = "device_limit"
)

// Reason is the detail code under a Category. The L0 values mirror
// ff-indexer-v2's media_viewability taxonomy string-for-string so a device
// record and an indexer row read the same; the play values are this
// package's own.
type Reason string

const (
	ReasonHTTPStatus        Reason = "http_status"
	ReasonDNS               Reason = "dns"
	ReasonTypeMismatch      Reason = "type_mismatch"
	ReasonContainerInvalid  Reason = "container_invalid"
	ReasonDirectoryListing  Reason = "directory_listing"
	ReasonKnownErrorPage    Reason = "known_error_page"
	ReasonZeroLength        Reason = "zero_length"
	ReasonTruncated         Reason = "truncated"
	ReasonInvalidURL        Reason = "invalid_url"
	ReasonUnsupportedScheme Reason = "unsupported_scheme"
	ReasonTransport         Reason = "transport"
	ReasonDataURIInvalid    Reason = "data_uri_invalid"
	// ReasonUnreachable: the origin gave no usable answer on every counted
	// attempt for a day (UnreachableDeadAfter) — no response or a server
	// error. A DNS-only streak concludes ReasonDNS instead.
	ReasonUnreachable Reason = "unreachable"

	// Play-layer reasons, one per outcome stage that can conclude.
	ReasonRenderDetect    Reason = "render_detect"
	ReasonRenderLoad      Reason = "render_load"
	ReasonRenderDecode    Reason = "render_decode"
	ReasonRenderWatchdog  Reason = "render_watchdog"
	ReasonRenderExhausted Reason = "render_exhausted"
	ReasonWebGLLost       Reason = "webgl_lost"

	// Reserved for the L1 render probe (its own PR); listed so the category
	// table is complete and pinned now.
	ReasonRenderBlank    Reason = "render_blank"
	ReasonRenderKnownBad Reason = "render_known_bad"
)

// CategoryOf maps a reason (plus the HTTP status behind an http_status
// reason) to its app-facing category. The table is closed: an unknown
// reason reads as CategoryRenderFailed for play reasons and
// CategoryNotMedia otherwise would be a guess, so it returns "" and the
// caller must treat that as a programming error (categories_test pins
// every Reason constant).
func CategoryOf(reason Reason, httpStatus int) Category {
	switch reason {
	case ReasonHTTPStatus:
		switch httpStatus {
		case 401, 403, 407, 429:
			// 429 only reaches a verdict from the PLAY layer (L0 keeps it
			// transient): the kiosk itself refused, play after play.
			return CategorySourceBlocked
		default:
			return CategorySourceMissing
		}
	case ReasonDNS, ReasonUnreachable:
		return CategorySourceMissing
	case ReasonTypeMismatch, ReasonDirectoryListing, ReasonKnownErrorPage:
		return CategoryNotMedia
	case ReasonZeroLength, ReasonTruncated, ReasonContainerInvalid:
		return CategoryCorruptFile
	case ReasonInvalidURL, ReasonUnsupportedScheme, ReasonDataURIInvalid, ReasonTransport:
		return CategoryUnsupported
	case ReasonRenderDetect, ReasonRenderLoad, ReasonRenderDecode, ReasonRenderWatchdog, ReasonRenderExhausted:
		return CategoryRenderFailed
	case ReasonWebGLLost:
		return CategoryDeviceLimit
	case ReasonRenderBlank, ReasonRenderKnownBad:
		return CategoryRenderBlank
	}
	return ""
}

// Recheck policy. Records never expire; these are how often a verdict is
// re-examined so a fixed source heals and a transient blip is retried.
const (
	// RecheckBroken: a broken verdict is re-probed after this while the
	// item is on the wall (the lookahead) or in the background sweep.
	RecheckBroken = 24 * time.Hour
	// RecheckBrokenBackoff replaces RecheckBroken once a record has been
	// re-probed broken BrokenBackoffAfter times in a row: a host that has
	// been gone for a week is not worth a daily request from every wall.
	RecheckBrokenBackoff = 7 * 24 * time.Hour
	BrokenBackoffAfter   = 7
	// RecheckOK: an ok verdict is re-probed weekly; a play success refreshes
	// it for free, so items that actually play are rarely re-probed.
	RecheckOK = 7 * 24 * time.Hour
	// RetryTransient: a transient attempt (no verdict) schedules a retry
	// after this; the stored verdict, if any, is untouched.
	RetryTransient = time.Hour
	// UnreachableDeadAfter / UnreachableDeadMinFailures: the one transient
	// class that concludes on its own (ProbeResult.Unreachable). A name that fails to resolve on every counted
	// attempt for a day (at least DNSDeadMinFailures attempts, each while
	// the device resolved other names) is broken, reason dns: the zone is
	// gone for this device whatever the resolver's excuse (an expired
	// domain answers SERVFAIL, not NXDOMAIN, on most resolvers). A single
	// SERVFAIL never is — it can be the device's own resolver. The verdict
	// keeps the normal broken recheck, so it heals the day the zone
	// returns.
	UnreachableDeadAfter       = 24 * time.Hour
	UnreachableDeadMinFailures = 3
)

// recordVersion is the record envelope version. Readers refuse a higher
// version (an older binary must not rewrite a record it does not understand)
// and migrate lower ones in Store.decode.
const recordVersion = 1

// Record is one source's observation history. Field names are the wire
// (bbolt value) contract: add fields, never rename.
type Record struct {
	V int `json:"v"`
	// Source is item.source exactly as pushed to the player: the hint key.
	// Device-only — see the package doc.
	Source string `json:"source"`
	// WireURL is the URL the L0 probe actually dialed (after the kiosk
	// gateway rewrite and any redirects), Host its host. Empty until L0 ran.
	WireURL string `json:"wireUrl,omitempty"`
	Host    string `json:"host,omitempty"`

	State    State    `json:"state,omitempty"`
	Category Category `json:"category,omitempty"`
	Reason   Reason   `json:"reason,omitempty"`
	Layer    Layer    `json:"layer,omitempty"`

	HTTPStatus   int    `json:"httpStatus,omitempty"`
	DeclaredType string `json:"declaredType,omitempty"`
	SniffedType  string `json:"sniffedType,omitempty"`
	MediaType    string `json:"mediaType,omitempty"`

	// CheckedAt is when the current State was set; RecheckAfter when it is
	// due for another look (the `recheck` bucket indexes this).
	CheckedAt    time.Time `json:"checkedAt"`
	RecheckAfter time.Time `json:"recheckAfter"`

	// LastError / LastErrorAt describe the most recent transient attempt.
	// They never change State. RetryAfter is when the next attempt may run
	// after one — tracked apart from RecheckAfter so a rate-limited origin
	// is left alone for RetryTransient WITHOUT extending an expired broken
	// verdict's authority to skip playback (Conclusive reads RecheckAfter
	// only).
	LastError   string     `json:"lastError,omitempty"`
	LastErrorAt *time.Time `json:"lastErrorAt,omitempty"`
	RetryAfter  time.Time  `json:"retryAfter,omitempty"`

	// UnreachableSince / UnreachableFailures track an unbroken streak of
	// attempts in which the origin gave no usable answer (DNS without
	// NXDOMAIN, no response, a server error), counted only while the
	// device reached other origins (ProbeResult.NetworkHealthy). Any
	// answer — alive, dead, a 401/403/429 — ends the streak. See
	// UnreachableDeadAfter for when it concludes.
	UnreachableSince    *time.Time `json:"unreachableSince,omitempty"`
	UnreachableFailures int        `json:"unreachableFailures,omitempty"`
	// legacyDNS* are the DNS-only streak fields an earlier build of this
	// branch wrote; Store.decode folds them into the unreachable streak
	// and they are never written again.
	LegacyDNSFailSince *time.Time `json:"dnsFailSince,omitempty"`
	LegacyDNSFailures  int        `json:"dnsFailures,omitempty"`

	// ConsecutiveBroken counts L0 re-probes that stayed broken, for the
	// recheck backoff. Reset by any ok.
	ConsecutiveBroken int `json:"consecutiveBroken,omitempty"`

	Plays PlayStats `json:"plays"`
	Build Build     `json:"build"`
}

// PlayStats is what the player reported about this source.
type PlayStats struct {
	OK      int `json:"ok"`
	Fail    int `json:"fail"`
	Skipped int `json:"skipped,omitempty"`
	// ConsecutiveFail counts failures since the last success; the
	// corroboration rule (Store.ApplyOutcome) concludes at 2.
	ConsecutiveFail int          `json:"consecutiveFail,omitempty"`
	LastOKAt        *time.Time   `json:"lastOkAt,omitempty"`
	LastFailAt      *time.Time   `json:"lastFailAt,omitempty"`
	LastFail        *PlayFailure `json:"lastFail,omitempty"`
}

// PlayFailure is the most recent failing outcome, kept whole so the app and
// the support bundle can say what the player actually saw.
type PlayFailure struct {
	Stage     string    `json:"stage"`
	Code      string    `json:"code,omitempty"`
	MediaType string    `json:"mediaType,omitempty"`
	Attempt   int       `json:"attempt,omitempty"`
	At        time.Time `json:"at"`
}

// Build records which software produced the current verdict.
type Build struct {
	Controld string `json:"controld,omitempty"`
	Player   string `json:"player,omitempty"`
}

// Conclusive reports whether the record's broken verdict is current enough
// to act on: hints and the admission verdict read ONLY this. A broken
// record past its recheck window is still shown to the app as broken (it
// is the last thing observed) but no longer skips the work — "no
// observation hides a work" means a stale one must not either.
func (r *Record) Conclusive(now time.Time) bool {
	return r != nil && r.State == StateBroken && now.Before(r.RecheckAfter)
}

// Due reports whether the record wants another look: past its recheck
// time AND past any transient retry hold.
func (r *Record) Due(now time.Time) bool {
	return r == nil || !now.Before(r.dueAt())
}

// dueAt is the later of the verdict recheck and the transient retry hold;
// the recheck index is keyed on it.
func (r *Record) dueAt() time.Time {
	if r.RetryAfter.After(r.RecheckAfter) {
		return r.RetryAfter
	}
	return r.RecheckAfter
}

// hostOf extracts the host of a URL for the record, "" when unparsable.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
