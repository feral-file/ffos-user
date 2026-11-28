package main

import (
	"net/http"
	"net/http/httputil"
)

func proxyHTTP(w http.ResponseWriter, r *http.Request) {
	p := httputil.ReverseProxy{Transport: transport, Rewrite: func(req *httputil.ProxyRequest) {
		req.Out.Host = req.Out.URL.Host
		req.Out.Header.Del("Proxy-Authorization")
	}}
	p.ServeHTTP(w, r)
}
