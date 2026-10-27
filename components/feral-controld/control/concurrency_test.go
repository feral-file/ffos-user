package control

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestConcurrentOpen races identical and distinct requests through one
// Authority: each ctr is admitted exactly once, whatever the interleaving.
func TestConcurrentOpen(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	type req struct {
		compact string
	}
	var reqs []req
	for range 40 {
		for _, c := range []*testCtl{owner, ctl} {
			reqs = append(reqs, req{d.seal(c, d.cmdClaims(c, "rotate", `{}`))})
		}
	}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for _, r := range reqs {
		for range 4 {
			wg.Add(1)
			go func(compact string) {
				defer wg.Done()
				adm, err := d.a.Open(compact)
				if err != nil {
					t.Error(err)
					return
				}
				if adm.Execute() {
					admitted.Add(1)
					if _, err := adm.Reply(true, "", map[string]bool{"ok": true}); err != nil {
						t.Error(err)
					}
				}
			}(r.compact)
		}
	}
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = d.a.SealAll("x", map[string]int{}) }()
	go func() { defer wg.Done(); _ = d.a.Status() }()
	wg.Wait()
	if got := admitted.Load(); got != int64(len(reqs)) {
		t.Fatalf("admitted %d, want %d", got, len(reqs))
	}
}
