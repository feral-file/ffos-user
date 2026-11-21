package main

// #include <stdlib.h>
import "C"

import (
	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
	"net/url"
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
