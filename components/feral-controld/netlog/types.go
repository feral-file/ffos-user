// Package netlog is the WAN-outage flight recorder
// (docs/wan-outage-observability.md stages 1-2): a JSONL ring under
// ~/.logs/netlog/ recording provisioning-machine transitions, monitord
// internet edges, relayer connect/disconnect/close-codes, and — on failure
// edges — the rate-limited diagnosis ladder's classification of WHY the
// device is offline.
//
// Boundary rules (the package's reason to exist depends on them):
//   - The recorder OBSERVES. It must never feed a recovery, AP-raise, or
//     reconnect decision; producers push events in and nothing reads netlog
//     state except the status/diagnostics egress paths.
//   - Classified outages are DATA, not errors: nothing here may log through
//     logger.Error (every Error becomes a Sentry event, and a flapping WAN
//     would turn the recorder into the noise it exists to diagnose).
//   - The ring lives under ~/.logs so uploadLogs bundles it with zero code
//     change (package rail only); its cap stays far below the 128 MB bundle
//     budget so it can never evict other logs.
package netlog

import "time"

// Stamp is the timestamp triple every record carries. Devices keep time by
// unauthenticated SNTP and boot with a wrong clock until sync, so wall time
// alone cannot order an outage that spans a clock step: UptimeS (CLOCK_BOOTTIME
// via /proc/uptime) plus BootID keep records orderable within a boot, and the
// wall clock ties them to the backend's timeline once sync has happened.
type Stamp struct {
	Wall    time.Time `json:"ts"`
	UptimeS float64   `json:"uptime_s"`
	BootID  string    `json:"boot_id,omitempty"`
}

// Record kinds. One JSONL line per record; Kind selects which payload
// pointer is set. Additive: readers must ignore unknown kinds/fields.
const (
	KindInternet    = "internet"     // monitord reachability verdict changed
	KindProv        = "prov"         // provisioning machine transition
	KindRelayer     = "relayer"      // relayer websocket connect/disconnect
	KindLadder      = "ladder"       // one diagnosis-ladder run
	KindOutageEnd   = "outage_end"   // an outage episode closed (reconnect)
	KindNote        = "note"         // recorder lifecycle / drop accounting
	KindUploadState = "upload_state" // self-upload attempts (stage 2a)
)

// Record is one JSONL line in the ring.
type Record struct {
	Stamp
	Kind string `json:"kind"`

	Internet *InternetEvent `json:"internet,omitempty"`
	Prov     *ProvEvent     `json:"prov,omitempty"`
	Relayer  *RelayerEvent  `json:"relayer,omitempty"`
	Ladder   *LadderResult  `json:"ladder,omitempty"`
	Outage   *OutageSummary `json:"outage,omitempty"`

	// Note carries lifecycle breadcrumbs (recorder_start, ladder_rate_limited,
	// self_upload_*, ...); Dropped counts producer events lost to a full queue
	// since the last note that reported them.
	Note    string `json:"note,omitempty"`
	Dropped int64  `json:"dropped,omitempty"`
}

// InternetEvent is one monitord reachability transition as seen by controld.
// Source distinguishes the edge signal from the level reconcile that healed a
// missed edge ("edge", "level", "initial") — diagnostic value: a timeline made
// entirely of "level" entries means edges are being lost.
type InternetEvent struct {
	Connected bool   `json:"connected"`
	Source    string `json:"source"`
}

// ProvEvent is one provisioning-machine transition (state or reason change).
type ProvEvent struct {
	From       string `json:"from"`
	To         string `json:"to"`
	FromReason string `json:"from_reason,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// RelayerEvent is one relayer connection transition. CloseCode is the
// websocket close code that ended the session when one was observed (0 =
// none seen — EOFs and dial failures carry no code).
type RelayerEvent struct {
	Connected bool `json:"connected"`
	CloseCode int  `json:"close_code,omitempty"`
}

// OutageSummary is the closed-episode summary: also served (converted) as the
// additive device_status `lastOutage` object (stage 2b), so the backend sees
// the diagnosis without pulling logs.
type OutageSummary struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Class    Class     `json:"class"`
	Count24h int       `json:"count24h"`
}
