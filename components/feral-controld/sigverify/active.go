package sigverify

import "sync"

// Active remembers the verdict of the playlist most recently ACCEPTED for
// the screen, so the status poller can annotate player_status with a
// signatureStatus for what is showing. It is a single slot, not a history:
// the player shows one playlist at a time, and a cast that fails never
// reaches Set. "Accepted" rather than "sent" on purpose: a displayAt-deferred
// cast is recorded at acceptance because its later scheduler cutovers have
// no hook of their own — see commandrouter's publication comment for the
// trade-off (the previous playlist loses its annotation until the cutover).
//
// Lookup matches by playlist id first and URL second because that is what
// the player's checkStatus reply echoes back (playlist.id for inline casts,
// playlistURL for URL casts). A miss means "controld did not verify what is
// on screen" — the player-fetched default playlist, the offline cached-copy
// fallback (which carries no verdict), a cast from before this process
// started, a still-showing playlist displaced from the slot by a pending
// schedule — and the poller omits the field rather than guess. Omission is
// always the safe direction: it can never assert a false verdict.
type Active struct {
	mu     sync.Mutex
	set    bool
	id     string
	url    string
	status Status
}

// Set records the verdict for the playlist just pushed. Either id or url may
// be empty; an entry with both empty is stored but can never be matched.
func (a *Active) Set(id, url string, status Status) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.set = true
	a.id = id
	a.url = url
	a.status = status
}

// Lookup returns the stored status when id or url identifies the stored
// playlist. Empty keys never match, so an on-screen playlist with neither an
// id nor a URL is a miss rather than a false hit on an empty stored key.
func (a *Active) Lookup(id, url string) (Status, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.set {
		return "", false
	}
	if id != "" && id == a.id {
		return a.status, true
	}
	if url != "" && url == a.url {
		return a.status, true
	}
	return "", false
}
