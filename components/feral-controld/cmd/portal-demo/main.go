// Command portal-demo serves the captive portal locally with stubbed
// provisioning seams, so portal changes (templates, copy, the shared
// stylesheet, fonts) can be reviewed in a browser — or from a phone on the
// same LAN — without a device advertising a hotspot.
//
//	go run ./cmd/portal-demo
//	open http://localhost:8080
//
// The seams are scripted for a full walk of every page and state:
//
//   - The scan returns three fake networks (the picker path; kill the Scan
//     stub to see the manual-SSID fallback).
//   - Joining with password "letmein" succeeds; any other password records
//     a wrong-password failure, so both /status banners are reachable.
//   - /rescan renders the confirm page; POST bounces to the rescan page.
//
// What this cannot exercise: captive-portal detection (the OS sign-in
// sheet), DNS/NAT redirection, and the real AP bounce — those live in the
// ffos image and NetworkManager, and need a device.
package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/feral-file/ffos-user/components/feral-controld/portal"
)

func main() {
	var mu sync.Mutex
	status := portal.Status{State: portal.JoinIdle}
	set := func(s portal.Status) {
		mu.Lock()
		defer mu.Unlock()
		status = s
	}

	srv := portal.NewServer(portal.Config{
		Addr:   "0.0.0.0:8080",
		APSSID: "FF1-DEMO4242",
		Scan: func(context.Context) ([]string, error) {
			return []string{"HomeNet-5G", "Cafe Wi-Fi", "Warehouse IoT"}, nil
		},
		Join: func(req portal.JoinRequest) error {
			if req.Password == "letmein" {
				set(portal.Status{
					State:   portal.JoinSucceeded,
					SSID:    req.SSID,
					Message: "Connected.",
				})
				return nil
			}
			set(portal.Status{
				State:   portal.JoinFailed,
				SSID:    req.SSID,
				Reason:  "auth-failure",
				Message: "Wrong Wi-Fi password. Please check it and try again.",
			})
			return fmt.Errorf("auth failure (scripted demo outcome)")
		},
		Status: func() portal.Status {
			mu.Lock()
			defer mu.Unlock()
			return status
		},
		Rescan: func() error { return nil },
	})
	if err := srv.Start(); err != nil {
		panic(err)
	}
	fmt.Println("portal demo on http://localhost:8080 (join password: letmein)")
	select {}
}
