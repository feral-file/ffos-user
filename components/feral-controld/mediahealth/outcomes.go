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
//
// The player numbers entries for the lifetime of its page, NOT per cast:
// a cast bumps castSeq but the ring keeps its entries and seq keeps
// counting, so the cursor must survive a cast — resetting it there would
// re-apply the entries still in the ring and let an ordinary cast
// manufacture its own corroboration. The cursor resets only when the
// numbering may have restarted: a player-session generation bump (Reset)
// or a ring whose highest seq is below the cursor (a restart the daemon
// did not see).
//
// A generation bump is not proof of a new page: a CDP transport reconnect
// bumps too, and the session re-stamps the document on every generation,
// so it cannot tell a surviving page from a replaced one. The drainer
// therefore also remembers the identity of every entry it consumed from
// the last ring's worth (seen). After a Reset, an entry still in the ring
// with a remembered identity is the same page talking and is skipped;
// only a ring holding none of them is a new page. Without this, a
// reconnect re-applied one failed play as a second failure in a distinct
// play — corroboration manufactured by the transport (PR #438 review,
// round 2, F1).
type Drainer struct {
	castSeq int64
	lastSeq int64
	// resetPending is set by Reset and cleared by the first Drain that
	// sees a non-empty ring and can classify it (same page or new page).
	resetPending bool
	// seen holds the identities of consumed entries, FIFO-bounded at
	// seenCap (two rings' worth, so one ring of fresh entries can never
	// evict the ring the reconnect still shows).
	seen  map[outcomeIdentity]struct{}
	order []outcomeIdentity
}

// outcomeIdentity is what makes one ring entry distinguishable from the
// same seq on a later page: the player's own timestamp (At) differs
// across pages. A player omitting At degrades to (seq, source, stage,
// attempt): a new page repeating exactly that tuple would be skipped once,
// which undercounts — the conservative side.
type outcomeIdentity struct {
	Seq     int64
	Source  string
	Kind    string
	Stage   string
	Attempt int
	At      string
}

// seenCap bounds the identity memory: the player keeps the last 64
// entries, so two rings' worth guarantees the ring shown after a reconnect
// is still remembered whatever was consumed meanwhile.
const seenCap = 128

// Reset marks the cursor as possibly stale: the player-session generation
// bumped. Whether the page actually restarted is decided by the next Drain
// against the remembered identities.
func (d *Drainer) Reset() {
	d.lastSeq = 0
	d.resetPending = true
}

// Drain returns the entries not yet seen, in seq order, and advances the
// cursor. gap is how many entries were lost between polls (the ring
// wrapped). restarted is true when this drain established that the page
// restarted its numbering (a sequence rollback, or a reset followed by a
// ring holding nothing remembered), so the caller's per-page state (the
// play run) should start over.
func (d *Drainer) Drain(o *Outcomes) (fresh []Outcome, gap int64, restarted bool) {
	if o == nil {
		return nil, 0, false
	}
	d.castSeq = o.CastSeq
	var maxSeq int64
	anySeen := false
	for _, e := range o.Entries {
		maxSeq = max(maxSeq, e.Seq)
		if _, ok := d.seen[identityOf(e)]; ok {
			anySeen = true
		}
	}
	if len(o.Entries) > 0 && maxSeq < d.lastSeq {
		// Sequence rollback: nothing in the ring is at or past the cursor,
		// so the page restarted its numbering. Everything present is new.
		d.lastSeq = 0
		restarted = true
	}
	if d.resetPending && len(o.Entries) > 0 {
		// The generation bump is now classifiable: a ring that still
		// holds consumed entries is the surviving page (a reconnect);
		// one holding none of them is a new page.
		d.resetPending = false
		restarted = restarted || !anySeen
	}
	if restarted {
		d.seen, d.order = nil, nil
	}
	entries := make([]Outcome, 0, len(o.Entries))
	for _, e := range o.Entries {
		if e.Seq <= d.lastSeq || e.Source == "" {
			continue
		}
		if _, ok := d.seen[identityOf(e)]; ok {
			continue
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Seq < entries[j].Seq })
	if len(entries) > 0 {
		if d.lastSeq > 0 && entries[0].Seq > d.lastSeq+1 {
			gap = entries[0].Seq - d.lastSeq - 1
		}
		for _, e := range entries {
			d.remember(identityOf(e))
		}
	}
	if len(o.Entries) > 0 {
		// Everything at or below the ring's top is now either consumed or
		// remembered, so the cursor can skip past it even when the fresh
		// set was empty (the reconnect case, where lastSeq was 0).
		d.lastSeq = max(d.lastSeq, maxSeq)
	}
	return entries, gap, restarted
}

func (d *Drainer) remember(id outcomeIdentity) {
	if d.seen == nil {
		d.seen = make(map[outcomeIdentity]struct{}, seenCap)
	}
	if _, ok := d.seen[id]; ok {
		return
	}
	d.seen[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > seenCap {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
}

func identityOf(e Outcome) outcomeIdentity {
	return outcomeIdentity{Seq: e.Seq, Source: e.Source, Kind: e.Kind, Stage: e.Stage, Attempt: e.Attempt, At: e.At}
}
