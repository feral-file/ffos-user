package control

import (
	"sync"
	"testing"
	"time"
)

// enrollWith sends a first-owner enroll carrying inv over route.
func (d *testDevice) enrollWith(c *testCtl, inv string, route Route) ReplyClaims {
	d.t.Helper()
	rc := d.claims(c, OpEnroll)
	rc.Enroll = c.enroll(inv)
	adm, err := d.a.OpenFrom(d.seal(c, rc), Origin{Route: route, Known: true})
	if err != nil {
		d.t.Fatal(err)
	}
	return d.final(adm, rc)
}

// §5.1: the QR rotates at half the invitation's life and the replaced
// invitation stays redeemable until its own expiry, so a code read off the
// screen always has invitationTTL/2 left.
func TestInvitationTwoSlots(t *testing.T) {
	const half = invitationTTL / 2
	lead := time.Duration(half) * time.Second
	type step struct {
		advance int64
		ensure  bool // repaint: EnsureInvitation(lead)
		clear   bool
	}
	tests := []struct {
		name  string
		steps []step
		// redeem is the index of the issued invitation to enroll with.
		redeem  int
		wantOK  bool
		wantLen int // invitations issued by the steps
	}{
		{name: "current redeems", steps: []step{{ensure: true}}, redeem: 0, wantOK: true, wantLen: 1},
		{name: "live invitation is reused", steps: []step{{ensure: true}, {advance: half - 1, ensure: true}}, redeem: 0, wantOK: true, wantLen: 1},
		{name: "prev redeems after one rotation", steps: []step{{ensure: true}, {advance: half, ensure: true}, {advance: half - 1}}, redeem: 0, wantOK: true, wantLen: 2},
		{name: "prev expires on its own clock", steps: []step{{ensure: true}, {advance: half, ensure: true}, {advance: half}}, redeem: 0, wantOK: false, wantLen: 2},
		{name: "current redeems after rotation", steps: []step{{ensure: true}, {advance: half, ensure: true}}, redeem: 1, wantOK: true, wantLen: 2},
		{name: "second rotation drops the oldest", steps: []step{{ensure: true}, {advance: half, ensure: true}, {advance: half, ensure: true}}, redeem: 0, wantOK: false, wantLen: 3},
		{name: "second rotation keeps the middle one", steps: []step{{ensure: true}, {advance: half, ensure: true}, {advance: half, ensure: true}}, redeem: 1, wantOK: true, wantLen: 3},
		{name: "clear voids both", steps: []step{{ensure: true}, {advance: half, ensure: true}, {clear: true}}, redeem: 0, wantOK: false, wantLen: 2},
		{name: "clear voids current", steps: []step{{ensure: true}, {advance: half, ensure: true}, {clear: true}}, redeem: 1, wantOK: false, wantLen: 2},
		{name: "expired current is not reused", steps: []step{{ensure: true}, {advance: invitationTTL, ensure: true}}, redeem: 0, wantOK: false, wantLen: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDevice(t)
			var issued []string
			for _, s := range tc.steps {
				d.advance(s.advance)
				if s.clear {
					d.a.ClearInvitation()
				}
				if !s.ensure {
					continue
				}
				inv, err := d.a.EnsureInvitation(lead)
				if err != nil {
					t.Fatal(err)
				}
				if inv.ExpiresAt-d.now.Load() < half {
					t.Fatalf("painted invitation has %d s left", inv.ExpiresAt-d.now.Load())
				}
				if len(issued) == 0 || issued[len(issued)-1] != inv.Value {
					issued = append(issued, inv.Value)
				}
			}
			if len(issued) != tc.wantLen {
				t.Fatalf("issued %d invitations, want %d", len(issued), tc.wantLen)
			}
			r := d.enrollWith(newTestCtl(t, "phone"), issued[tc.redeem], RouteLAN)
			if r.OK != tc.wantOK || (!r.OK && r.Code != CodeInvitationInvalid) {
				t.Fatalf("enroll: ok=%v code=%q, want ok=%v", r.OK, r.Code, tc.wantOK)
			}
		})
	}
}

// A successful first-owner enroll consumes both slots: neither the code it
// used nor the other one makes a second owner.
func TestInvitationEnrollConsumesBoth(t *testing.T) {
	for _, use := range []int{0, 1} {
		d := newTestDevice(t)
		first, _ := d.a.IssueInvitation()
		second, _ := d.a.IssueInvitation()
		both := []string{first.Value, second.Value}
		if r := d.enrollWith(newTestCtl(t, "phone"), both[use], RouteLAN); !r.OK {
			t.Fatalf("enroll with slot %d: %+v", use, r)
		}
		if _, live := d.a.CurrentInvitation(); live {
			t.Fatal("invitation live after the owner enrolled")
		}
		if r := d.enrollWith(newTestCtl(t, "tablet"), both[1-use], RouteLAN); r.Code != CodeInvitationInvalid {
			t.Fatalf("other slot after enroll: %+v", r)
		}
	}
}

// Painters racing through EnsureInvitation rotate once: a second rotation
// would push the invitation the first painter put on screen out of prev.
func TestEnsureInvitationConcurrentPaintsRotateOnce(t *testing.T) {
	d := newTestDevice(t)
	old, _ := d.a.IssueInvitation()
	d.advance(invitationTTL / 2)
	const painters = 16
	got := make([]string, painters)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inv, err := d.a.EnsureInvitation(time.Duration(invitationTTL/2) * time.Second)
			if err != nil {
				t.Error(err)
			}
			got[i] = inv.Value
		}()
	}
	wg.Wait()
	for _, v := range got {
		if v != got[0] || v == old.Value {
			t.Fatalf("painters disagree or reused the old invitation: %v", got)
		}
	}
	if r := d.enrollWith(newTestCtl(t, "phone"), old.Value, RouteLAN); !r.OK {
		t.Fatalf("replaced invitation not redeemable after concurrent paints: %+v", r)
	}
}

func TestEnsureInvitationRefusedWhenEnrolled(t *testing.T) {
	d, _, _ := ownerDevice(t)
	if _, err := d.a.EnsureInvitation(time.Minute); err == nil {
		t.Fatal("invitation issued on an enrolled device")
	}
}

// §5.1: the invitation is the whole first-owner credential on every route,
// the relay included; an owned device refuses it everywhere.
func TestFirstOwnerEnrollRoutes(t *testing.T) {
	for _, route := range []Route{RouteLAN, RouteRelayer, RouteRelayV2} {
		d := newTestDevice(t)
		inv, err := d.a.IssueInvitation()
		if err != nil {
			t.Fatal(err)
		}
		if r := d.enrollWith(newTestCtl(t, "stranger"), "AAAAAAAAAAAAAAAAAAAAAA", route); r.Code != CodeInvitationInvalid {
			t.Fatalf("route %v, wrong invitation: %+v", route, r)
		}
		if _, live := d.a.CurrentInvitation(); !live {
			t.Fatalf("route %v: a wrong guess voided the invitation", route)
		}
		if r := d.enrollWith(newTestCtl(t, "phone"), inv.Value, route); !r.OK || d.a.State() != StateEnrolled {
			t.Fatalf("route %v, legacy: %+v", route, r)
		}
		if r := d.enrollWith(newTestCtl(t, "tablet"), inv.Value, route); r.Code != CodeInvitationInvalid {
			t.Fatalf("route %v, enrolled: %+v", route, r)
		}
	}
}
