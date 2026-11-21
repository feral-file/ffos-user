package main

// #include <stdlib.h>
import "C"

import (
	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
	"net/url"
	"strings"
)

// The caller already matched the exact configured host. Keep the raw path
// and query because a generative work can use either as its seed.
//
//export ff_gateway_url
func ff_gateway_url(raw, gateway *C.char) *C.char {
	u, err := url.Parse(C.GoString(raw))
	if err != nil {
		return nil
	}
	cid, rest, ok := contentpath.IPFS(u.EscapedPath())
	if !ok {
		return nil
	}
	target := C.GoString(gateway)
	if target == "" {
		return nil
	}
	path := "/ipfs/" + cid + rest
	if u.RawQuery != "" || u.ForceQuery {
		if rest == "" {
			path += "/"
		}
		path += "?" + u.RawQuery
	}
	return C.CString(target + path)
}

//export ff_observed_gateway_url
func ff_observed_gateway_url(source, observed, raw *C.char) *C.char {
	original, err := url.Parse(C.GoString(source))
	if err != nil {
		return nil
	}
	target, err := url.Parse(C.GoString(observed))
	if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil {
		return nil
	}
	request, err := url.Parse(C.GoString(raw))
	if err != nil || !strings.EqualFold(request.Host, original.Host) || request.Scheme != original.Scheme {
		return nil
	}
	// A work may embed another CID on the same gateway. Only route resources
	// belonging to the content for which the probe observed this fallback.
	for i, parse := range []func(string) (string, string, bool){contentpath.IPFS, contentpath.Arweave} {
		id, _, ok := parse(original.EscapedPath())
		tid, _, tok := parse(target.EscapedPath())
		rid, rest, rok := parse(request.EscapedPath())
		if !ok || !tok || !rok || id != rid || id != tid {
			continue
		}
		prefix := "/"
		if i == 0 {
			prefix = "/ipfs/"
		}
		if rest == "" && request.RawQuery != "" {
			rest = "/"
		}
		result := "https://" + target.Host + prefix + id + rest
		if request.RawQuery != "" || request.ForceQuery {
			result += "?" + request.RawQuery
		}
		return C.CString(result)
	}
	return nil
}
