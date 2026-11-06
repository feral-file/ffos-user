package mediahealth

import (
	"net/url"
	"regexp"
	"strings"
)

// Network is a content-addressed network whose paths name content, not a
// location: the same id is served by any gateway that can retrieve it.
type Network string

const (
	NetworkIPFS    Network = "ipfs"
	NetworkArweave Network = "ar"
)

// Default gateway lists (controld.json mediaHealth.ipfsGateways /
// arweaveGateways), ordered: tried one by one after the source's own
// gateway, stopping at the first that serves. Measured from the office FF1
// on 2026-11-06 (three fixture CIDs, native fetch): filebase 0.4–4.3 s,
// pinata 5.5–8.8 s; w3s.link, dweb.link and ipfs.io answered 429 to every
// request, 4everland timed out on 2 of 3, cloudflare-ipfs.com is gone, and
// ipfs.feralfile.com serves only what Feral File pinned. See the locked
// design on #392 (issuecomment-6683248733).
//
// Arweave ships with arweave.net ALONE: a fallback for sources on any
// other Arweave gateway, none for sources on arweave.net itself (a
// gateway never confirms its own 404). ar-io.dev was dropped on
// 2026-11-06: it serves a 32 KiB range of a 195 MB video but answers the
// kiosk's whole-file request with 402 Payment Required (an x402 offer
// for "data egress"), so every item routed through it failed to play.
// Add a fallback here only after a WHOLE-FILE load of a large work has
// been seen to succeed from the device.
var (
	DefaultIPFSGateways    = []string{"https://ipfs.filebase.io", "https://gateway.pinata.cloud"}
	DefaultArweaveGateways = []string{"https://arweave.net"}
)

// maxConfirmGateways bounds the extra requests one probe may spend on a
// gateway 404: the item deadline is the real bound, this keeps the request
// budget predictable.
const maxConfirmGateways = 2

// Gateways is the ordered fallback list per network, as origins
// ("https://host"). Immutable after NewGateways; safe for concurrent use.
type Gateways struct {
	lists map[Network][]string
	// arweaveHosts are the hosts whose first path segment is a transaction
	// id: the configured Arweave gateways plus the well-known ones.
	arweaveHosts map[string]bool
}

// NewGateways normalizes two operator lists to https origins, dropping
// anything that is not an https URL with a host. An empty or fully
// invalid list falls back to its default.
func NewGateways(ipfs, arweave []string) *Gateways {
	g := &Gateways{
		lists:        map[Network][]string{NetworkIPFS: origins(ipfs, DefaultIPFSGateways), NetworkArweave: origins(arweave, DefaultArweaveGateways)},
		arweaveHosts: map[string]bool{"arweave.net": true, "ar-io.dev": true, "ar-io.net": true},
	}
	for _, o := range g.lists[NetworkArweave] {
		g.arweaveHosts[hostOf(o)] = true
	}
	return g
}

func origins(list, fallback []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range list {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			continue
		}
		o := "https://" + strings.ToLower(u.Host)
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), fallback...)
	}
	return out
}

// arweaveTxID matches an Arweave transaction id: 32 bytes, base64url,
// no padding (43 characters).
var arweaveTxID = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// ipfsCID matches a CID as it appears in a gateway path: CIDv0 (base58,
// "Qm…", 46 characters) or a CIDv1 in its common lowercase multibase
// forms. Loose on purpose: a false positive costs one extra gateway
// request, a false negative only skips the fallback.
var ipfsCID = regexp.MustCompile(`^(Qm[1-9A-HJ-NP-Za-km-z]{44}|b[a-z2-7]{50,}|z[1-9A-HJ-NP-Za-km-z]{40,}|f[0-9a-f]{40,})$`)

// ContentRef is a content-addressed URL taken apart: which network, the
// content id, and the path and query after it (kept verbatim, so a work's
// own files under its CID or transaction follow the same gateway).
type ContentRef struct {
	Network Network
	ID      string
	// Rest is the escaped path after the id ("" or "/index.html"), Query
	// the raw query.
	Rest  string
	Query string
	Host  string
}

// Key is the gateway-independent identity of the content ("ar:<tx>",
// "ipfs:<cid>").
func (r ContentRef) Key() string { return string(r.Network) + ":" + r.ID }

// RouteKey scopes a fallback-gateway choice to the content AND the gateway
// it was requested through ("ipfs:<cid>@ipfs.pixura.io"): every request
// under that CID or transaction on that gateway — the source and the
// work's own files — is routed alike. Which sources NEED the route is
// tracked per source (Store.servedBy): the route stays while any of them
// does, so one source recovering never drops another's fallback, whether
// the other names the same CID through a different gateway or a different
// file under the same CID on the same one (PR #438 review, F25).
func (r ContentRef) RouteKey() string { return r.Key() + "@" + r.Host }

// Ref parses a URL as content-addressed: `/ipfs/<cid>…` on any host, or
// `/<tx>…` on an Arweave gateway host. ok is false for anything else.
func (g *Gateways) Ref(rawURL string) (ContentRef, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return ContentRef{}, false
	}
	host := strings.ToLower(u.Hostname())
	path := u.EscapedPath()
	if rest, found := strings.CutPrefix(path, "/ipfs/"); found {
		id, tail, _ := strings.Cut(rest, "/")
		if ipfsCID.MatchString(id) {
			if tail != "" || strings.HasSuffix(rest, "/") {
				tail = "/" + tail
			}
			return ContentRef{Network: NetworkIPFS, ID: id, Rest: tail, Query: u.RawQuery, Host: host}, true
		}
		return ContentRef{}, false
	}
	if g != nil && g.arweaveHosts[host] {
		id, tail, hasTail := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if arweaveTxID.MatchString(id) {
			if hasTail {
				tail = "/" + tail
			}
			return ContentRef{Network: NetworkArweave, ID: id, Rest: tail, Query: u.RawQuery, Host: host}, true
		}
	}
	return ContentRef{}, false
}

// URLOn builds the same content on origin: the network's path prefix, the
// id, the rest and the query, verbatim. Never anything from the request
// but its own path and query — the origin always comes from the operator's
// configured list.
func (r ContentRef) URLOn(origin string) string {
	prefix := "/"
	if r.Network == NetworkIPFS {
		prefix = "/ipfs/"
	}
	out := origin + prefix + r.ID + r.Rest
	if r.Query != "" {
		out += "?" + r.Query
	}
	return out
}

// candidates are the gateways to try after the source's own, in order,
// skipping the host already tried, at most maxConfirmGateways.
func (g *Gateways) candidates(ref ContentRef, triedHost string) []string {
	var out []string
	for _, o := range g.lists[ref.Network] {
		if strings.EqualFold(hostOf(o), triedHost) || strings.EqualFold(hostOf(o), ref.Host) {
			continue
		}
		out = append(out, o)
		if len(out) == maxConfirmGateways {
			break
		}
	}
	return out
}

// Hosts lists every configured gateway host per network (for the kiosk
// interceptor's Fetch patterns).
func (g *Gateways) ArweaveHosts() []string {
	out := make([]string, 0, len(g.arweaveHosts))
	for h := range g.arweaveHosts {
		out = append(out, h)
	}
	return out
}
