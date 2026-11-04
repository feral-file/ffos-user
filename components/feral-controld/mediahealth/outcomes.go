package mediahealth

import (
	"sort"
)

// Outcomes is the `outcomes` field of the player's checkStatus reply under
// the `mediaHealth` v1 manifest contract: a bounded ring of the most recent
// per-item play outcomes, failures AND successes. Live-only on the player
// (boot and hydrate strip it); CastSeq increments on every cast and Seq on
// every entry, so a reader can tell a new cast from a continuation and a
// dropped entry from a quiet wall.
type Outcomes struct {
	CastSeq int64     `json:"castSeq"`
	Entries []Outcome `json:"entries"`
}

// Outcome is one entry.
type Outcome struct {
	Seq    int64  `json:"seq"`
	Source string `json:"source"`
	// Kind is ok | fail | skip.
	Kind string `json:"kind"`
	// Stage names where the outcome was observed (see the Stage constants).
	Stage string `json:"stage"`
	// Code is stage-specific: the HEAD status or "network" for detect, the
	// element kind for load, the MediaError name for decode, the deadline
	// for watchdog.
	Code      string `json:"code,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	ElapsedMs int64  `json:"elapsedMs,omitempty"`
	// At is the player's own ISO timestamp, kept as reported.
	At string `json:"at,omitempty"`
}

const (
	OutcomeOK   = "ok"
	OutcomeFail = "fail"
	OutcomeSkip = "skip"

	// Stages. StageInferred is controld's own: a renderStatus==3 sighting on
	// a player without the contract.
	StageDetect    = "detect"
	StageLoad      = "load"
	StageDecode    = "decode"
	StageWatchdog  = "watchdog"
	StageWebGL     = "webgl"
	StageExhausted = "exhausted"
	StageHint      = "hint"
	StageReady     = "ready"
	StageInferred  = "inferred"

	// CodeNetwork is the detect-stage code the player reports when its
	// content-type HEAD failed at the transport layer (no HTTP answer at
	// all). The player raises it only once it has corroborated that the
	// device is offline, so it describes the device's network, not the
	// source.
	CodeNetwork = "network"
)

// deviceLocal reports whether a failing outcome describes the device rather
// than the source: a lost WebGL context, or a detect-stage transport failure
// (the device is offline). Such outcomes are recorded on the play stats for
// the app and the support bundle but never advance a verdict — counting
// them would let an outage mark every work on the wall broken, and a
// conclusive broken record is skipped before it is mounted, so the play
// layer could never heal it once the network returned.
func deviceLocal(o Outcome) bool {
	switch o.Stage {
	case StageWebGL:
		return true
	case StageDetect:
		return o.Code == CodeNetwork
	}
	return false
}

// reasonForStage maps a failing stage to the record reason; "" for a stage
// that is not a failure of the source (hint, ready) or unknown.
func reasonForStage(stage string) Reason {
	switch stage {
	case StageDetect:
		return ReasonRenderDetect
	case StageLoad:
		return ReasonRenderLoad
	case StageDecode:
		return ReasonRenderDecode
	case StageWatchdog:
		return ReasonRenderWatchdog
	case StageExhausted, StageInferred:
		return ReasonRenderExhausted
	case StageWebGL:
		return ReasonWebGLLost
	}
	return ""
}

// Drainer tracks what has already been read from the ring so each outcome
// is applied once. One per poller; not safe for concurrent use.
type Drainer struct {
	castSeq int64
	lastSeq int64
	primed  bool
}

// Drain returns the entries of o not yet seen, in Seq order, and advances
// the cursor. A castSeq change resets the cursor (a new cast restarts seq
// numbering). A gap between the last seen seq and the oldest returned entry
// means the ring wrapped between polls; it is reported in gap so the caller
// can log it.
func (d *Drainer) Drain(o *Outcomes) (fresh []Outcome, gap int64) {
	if o == nil {
		return nil, 0
	}
	if !d.primed || o.CastSeq != d.castSeq {
		d.castSeq = o.CastSeq
		d.lastSeq = 0
		d.primed = true
	}
	entries := make([]Outcome, 0, len(o.Entries))
	for _, e := range o.Entries {
		if e.Seq > d.lastSeq && e.Source != "" {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
	if len(entries) > 0 {
		if d.lastSeq > 0 && entries[0].Seq > d.lastSeq+1 {
			gap = entries[0].Seq - d.lastSeq - 1
		}
		d.lastSeq = entries[len(entries)-1].Seq
	}
	return entries, gap
}
