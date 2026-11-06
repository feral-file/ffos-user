package mediahealth

import (
	"net/http"
	"time"
)

// Issue is the latest real attempt's problem with a source that has NO
// verdict saying so (#392, "report temporary failures honestly"): the
// app shows it beside `state`, never instead of it. Before it, a
// temporary failure read `unknown`, exactly like a source never checked,
// so a work that had failed every attempt for an hour looked to the app
// like one the device had not reached yet.
//
// A CLOSED vocabulary the app renders labels for, like Category: add a
// value only with its row in docs/media-health.md and its mapping in
// issueForProbe / the outcome fold. Never derived from, and never carrying,
// the raw lastError text — that holds the host and path, and every
// surface is URL-free.
type Issue string

const (
	// IssueNoAnswer: a background probe got no response at all (timeout
	// before any header, refused or reset connection, TLS failure).
	IssueNoAnswer Issue = "no_answer"
	// IssueDNS: the name did not resolve, short of a definitive NXDOMAIN
	// (which is a broken verdict at once).
	IssueDNS Issue = "dns"
	// IssueServerError: a 5xx without Retry-After.
	IssueServerError Issue = "server_error"
	// IssueRateLimited: 429, or a 503 that names a Retry-After.
	IssueRateLimited Issue = "rate_limited"
	// IssueAccessDenied: 401/403/407 to the probe — possibly the probe's
	// identity rather than the work; playback decides.
	IssueAccessDenied Issue = "access_denied"
	// IssuePlayFailed: one counted failed play (from the player's ring or
	// the kiosk observer), not yet the two a verdict needs.
	IssuePlayFailed Issue = "play_failed"
)

// probeSide reports whether an issue came from the probe (as opposed to a
// play): those are cleared by the kiosk serving the document (a 2xx seen
// by the navigation observer) — the kiosk's own evidence wins over the
// probe's.
func (i Issue) probeSide() bool {
	switch i {
	case IssueNoAnswer, IssueDNS, IssueServerError, IssueRateLimited, IssueAccessDenied:
		return true
	}
	return false
}

// issueForProbe maps a transient probe result to its issue, or "" when the
// result says nothing reportable: an unreachable result while the device
// reached no other origin (an outage is not this source's problem), and
// the transient statuses that are about the probe's request rather than
// the source (406, 408, 416).
func issueForProbe(res ProbeResult) Issue {
	if res.GatewayFailed {
		// One gateway failed to serve a transaction another serves.
		return IssueServerError
	}
	switch res.Unreachable {
	case UnreachableDNS:
		if res.NetworkHealthy {
			return IssueDNS
		}
		return ""
	case UnreachableNoAnswer:
		if res.NetworkHealthy {
			return IssueNoAnswer
		}
		return ""
	case UnreachableServer:
		if res.NetworkHealthy {
			return IssueServerError
		}
		return ""
	}
	switch res.Status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return IssueRateLimited
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired:
		return IssueAccessDenied
	}
	return ""
}

// setIssue records issue at now. issueSince marks when the current run
// of trouble began, so it is kept across a change of KIND (a source that
// went from no_answer to server_error has been in trouble since the
// first), and reset only when the issue clears.
func (r *Record) setIssue(issue Issue, now time.Time) {
	if issue == "" {
		return
	}
	if r.Issue == "" || r.IssueSince == nil {
		r.IssueSince = new(now)
	}
	r.Issue = issue
}

// clearIssue drops any issue (a real answer, a verdict).
// dropRoute forgets the fallback gateway a broken work was routed
// through. Every broken conclusion calls it, whichever layer concluded:
// a play-layer verdict over a work "served by" a fallback left the route
// standing, so the kiosk kept loading the gateway the verdict had just
// proven does not play it (ar-io.dev's 402s, office FF1, 2026-11-06). A
// broken work loads from the source's own gateway again until a probe
// finds a gateway that serves it.
func (r *Record) dropRoute() { r.ServedBy = "" }

func (r *Record) clearIssue() {
	r.Issue, r.IssueSince = "", nil
}
