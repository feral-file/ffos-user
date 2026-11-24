package mediahealth

import (
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
)

// A web3 source is device-held for the same reason an onchfs one is: there is
// no origin to probe, so a failure to show it says nothing about the network
// and must never conclude a verdict (feral-file#3599). IsDeviceHeld is the
// one predicate every origin-judging pass asks, so a scheme added later is
// covered by adding it to contentpath, not by finding these call sites again.
func TestIsDeviceHeldCoversWeb3(t *testing.T) {
	web3, ok := contentpath.ParseWeb3("web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/artpiece")
	if !ok {
		t.Fatal("ParseWeb3 refused the Digital Soul URI")
	}
	onchfs, ok := contentpath.ParseOnchfs("onchfs://1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a/?fxhash=1")
	if !ok {
		t.Fatal("ParseOnchfs refused")
	}
	held := map[string]bool{
		web3.SourceURL():                   true,
		onchfs.SourceURL():                 true,
		"data:text/html,<p>hi":             true,
		"https://cdn.example/work.html":    false,
		"https://ipfs.example/ipfs/bafy/x": false,
		// A raw URI never reaches a probe: dp1 maps it before the player
		// sees it. If one did it is not device-held, and the source guard
		// refuses the scheme anyway.
		"web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/artpiece": false,
	}
	for source, want := range held {
		if got := IsDeviceHeld(source); got != want {
			t.Errorf("IsDeviceHeld(%q) = %v, want %v", source, got, want)
		}
	}
}

// A device-held source is answered inconclusive without a dial, at the top of
// the probe, so an RPC outage can never mark one of these works broken.
func TestProbeDoesNotDialADeviceHeldSource(t *testing.T) {
	web3, ok := contentpath.ParseWeb3("web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/artpiece")
	if !ok {
		t.Fatal("ParseWeb3 refused")
	}
	p := &Probe{}
	res := p.ProbeOneWithin(t.Context(), web3.SourceURL(), 0)
	if res.Verdict != VerdictInconclusive {
		t.Fatalf("Verdict = %v, want inconclusive", res.Verdict)
	}
	if res.Err == nil {
		t.Fatal("a device-held probe must carry errDeviceHeld so it is never recorded")
	}
}
