package sigverify

import "sync"

// Active remembers the signature verdict of the playlist on screen, so the
// status poller can annotate player_status with a signatureStatus for it.
// Two slots, no history:
//
//   - current is the verdict of the playlist last pushed to the player and
//     accepted. Set by a cast or refresher re-push that reached the screen,
//     inside the player-push critical section so it is ordered against every
//     other push. A push that reached the screen WITHOUT a verdict (the
//     offline cached-copy fallback) clears it: a previous cast's verdict must
//     never keep standing for bytes nobody verified, and both share the URL.
//   - pending is the verdict of a displayAt-deferred cast (or a future-only
//     refresh of the schedule) whose first cohort has not reached the player
//     yet. The previous playlist keeps showing, so current is left alone.
//     Promote, called by the scheduler's push observer after a cohort push
//     the player accepted, copies pending into current; it is idempotent so
//     later cohorts of the same schedule are harmless. A deferred document
//     WITHOUT a verdict (the cached-copy fallback) parks an explicit
//     "unverified" pending, so its promotion CLEARS current rather than
//     letting an earlier document's verdict survive the cutover. Every
//     producer that replaces the schedule must restage pending — the cast
//     handler and the refresher's future-only path both do — or a cutover
//     would promote the verdict of a document the schedule no longer holds.
//     A fresh cast (Set/Clear) drops pending, since it replaced the schedule.
//
// Lookup consults current only. Identity is the playlist id when both the
// reply and the slot carry one — two documents republished at the same URL
// have different ids, and the URL must not attribute one's verdict to the
// other. The URL is consulted only when either side lacks an id (the
// player's checkStatus reply echoes playlist.id for inline casts and
// playlistURL for URL casts, not always both). A miss means "controld did not
// verify what is on screen" — the player-fetched default playlist, the
// cached-copy fallback, a cast from before this process started, a deferred
// cast whose cutover has not happened — and the poller omits the field rather
// than guess. Omission is always the safe direction: it can never assert a
// verdict for a document other than the one it was computed on.
type Active struct {
	mu      sync.Mutex
	current slot
	pending slot
}

type slot struct {
	set    bool
	id     string
	url    string
	status Status
	// unverified marks a pending slot whose promotion must clear current:
	// the scheduled document has no verdict. Never set on current.
	unverified bool
	// idOnly disables the URL fallback on current: two documents with
	// different verdicts share this URL (a soft refresh replaced one with
	// the other), so only a reply naming the id can be attributed. Cleared
	// by the next Set.
	idOnly bool
}

func (s slot) matches(id, url string) bool {
	if !s.set {
		return false
	}
	// Both sides know the id: it decides, and a mismatch is a miss even on
	// the same URL (a republished document is a different document).
	if id != "" && s.id != "" {
		return id == s.id
	}
	if s.idOnly {
		return false
	}
	// Empty keys never match, so an on-screen playlist with neither an id
	// nor a URL is a miss rather than a false hit on an empty stored key.
	return url != "" && url == s.url
}

// Set records the verdict of a playlist that just reached the player, and
// drops any pending schedule verdict (the cast replaced the schedule).
func (a *Active) Set(id, url string, status Status) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.current = slot{set: true, id: id, url: url, status: status}
	a.pending = slot{}
}

// Clear records that a playlist WITHOUT a verdict just reached the player:
// nothing on screen is verified any more. Also drops any pending verdict.
func (a *Active) Clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.current = slot{}
	a.pending = slot{}
}

// ClearCurrent records that player-owned content replaced what controld
// last pushed — the player reloaded (CDP reconnect) or displayDefaultPlaylist
// was accepted — without touching pending: the scheduler still holds its
// schedule and re-pushes it after a reconnect, and that push's promotion
// restores the annotation.
func (a *Active) ClearCurrent() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.current = slot{}
}

// SetPending parks the verdict of a displayAt-deferred cast until Promote.
// current is untouched: the previous playlist is still what is showing.
func (a *Active) SetPending(id, url string, status Status) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = slot{set: true, id: id, url: url, status: status}
}

// SetPendingUnverified parks "the scheduled document has no verdict" (a
// deferred cast or future-only refresh served from the cached copy): when
// its cohort reaches the player, Promote clears current instead of leaving
// the previous document's verdict standing.
func (a *Active) SetPendingUnverified() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = slot{set: true, unverified: true}
}

// ReconcileSoft records the outcome of a SOFT refresh (refresh:true): the
// player accepted the document but may keep the current item on screen
// until it ends, so acceptance alone does not prove the new document is
// showing. Three cases, each honest about that ambiguity:
//
//   - the verdict is unchanged: current is refreshed to the new identity.
//     Whichever document is showing, the status reported is true of it.
//   - both documents carry ids, they differ, and the verdict changed:
//     current keeps the old document's verdict but becomes id-only. A reply
//     naming the old id is a hit while it still shows and one naming the new
//     id is a miss; a URL-only reply — which cannot say which document it
//     describes — is a miss too, until a force cast or cutover sets a fresh
//     slot.
//   - identity cannot separate them (URL-only, or same id) and the verdict
//     changed: current is cleared. Nothing may attest a status that is true
//     of only one of two documents the poller cannot tell apart.
//
// A soft refresh never touches pending: it replaces no schedule.
func (a *Active) ReconcileSoft(id, url string, status Status) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.current.set {
		// Nothing was attested; a soft refresh cannot establish a first
		// attestation either (the old, unattested document may still show).
		return
	}
	if a.current.status == status {
		a.current = slot{set: true, id: id, url: url, status: status}
		return
	}
	if id != "" && a.current.id != "" && id != a.current.id {
		a.current.idOnly = true
		return
	}
	a.current = slot{}
}

// Promote applies the pending state to current. Called once a scheduler-owned
// push reached the player. No-op when nothing is pending; idempotent.
func (a *Active) Promote() {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case !a.pending.set:
	case a.pending.unverified:
		a.current = slot{}
	default:
		a.current = a.pending
	}
}

// Lookup returns the current status when id or url identifies the current
// playlist. Pending is never consulted.
func (a *Active) Lookup(id, url string) (Status, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current.matches(id, url) {
		return a.current.status, true
	}
	return "", false
}
