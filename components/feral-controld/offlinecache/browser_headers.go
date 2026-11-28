package offlinecache

import (
	"net/http"
	"sort"
	"strings"
)

// HeaderField preserves repeated browser security policies. Combining two CSP
// fields or keeping only the first changes the policy enforced by Chromium.
type HeaderField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func browserReplayHeaders(raw map[string]string) ([]HeaderField, bool) {
	var fields []HeaderField
	supported := true
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	total := 0
	for _, name := range names {
		canonical := http.CanonicalHeaderKey(name)
		value := raw[name]
		total += len(name) + len(value)
		if total > 64<<10 {
			return nil, false
		}
		switch canonical {
		case "Content-Length", "Content-Encoding", "Transfer-Encoding", "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Upgrade":
			continue
		case "Set-Cookie":
			supported = false
			continue
		case "Vary":
			for _, variant := range strings.Split(value, ",") {
				if !strings.EqualFold(strings.TrimSpace(variant), "Accept-Encoding") {
					supported = false
				}
			}
		}
		if strings.ContainsAny(name, "\r\n:") {
			return nil, false
		}
		for _, part := range strings.Split(value, "\n") {
			if strings.ContainsRune(part, '\r') {
				return nil, false
			}
			fields = append(fields, HeaderField{canonical, part})
		}
	}
	return fields, supported
}

// HTTP-refetched bytes must carry the policy of that same response. A redirect
// to another URL cannot be flattened into a browser document at the old origin.
func (r *Resource) captureBodyHeaders(resp *http.Response) {
	r.BrowserPolicyVersion = 0
	r.HeaderFields = nil
	if resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.String() != r.URL || resp.StatusCode != http.StatusOK {
		return
	}
	raw := make(map[string]string, len(resp.Header))
	for name, vs := range resp.Header {
		raw[name] = strings.Join(vs, "\n")
	}
	if fields, ok := browserReplayHeaders(raw); ok {
		r.BrowserPolicyVersion = 2
		r.HeaderFields = fields
	}
}
