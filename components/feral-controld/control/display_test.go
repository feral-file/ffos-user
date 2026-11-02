package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const testCast = `{"intent":{"action":"now_display"},"dp1_call":{"items":[]}}`

func displayReq(c *testCtl, origin string, attested bool) DisplayGrantRequest {
	return DisplayGrantRequest{Sig: c.sigPub, Enc: c.encPub, Origin: origin, OriginAttested: attested}
}

// issue issues a display grant for site, failing the test on error.
func (d *testDevice) issue(approver string, site *testCtl, origin string) DisplayGrant {
	d.t.Helper()
	g, err := d.a.IssueDisplayGrant(approver, displayReq(site, origin, true))
	if err != nil {
		d.t.Fatalf("IssueDisplayGrant: %v", err)
	}
	return g
}

func (d *testDevice) displayFile() string {
	b, err := d.fs.ReadFile(d.dir + "/" + displayGrantsFile)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestDisplayGrantIssuanceByState(t *testing.T) {
	site := newTestCtl(t, "site")
	// legacy: no approver, approvedBy "".
	d := newTestDevice(t)
	g := d.issue("", site, "https://a.example")
	if g.ApprovedBy != "" || g.Label != "a.example" || !g.Active(testNow) || g.Ctl != site.ctl {
		t.Fatalf("legacy grant = %+v", g)
	}
	if _, err := d.a.IssueDisplayGrant("nobody", displayReq(newTestCtl(t, "x"), "https://b.example", true)); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("unknown approver in legacy: %v", err)
	}
	// enrolled and enforced: the approver must be an active owner.
	for _, enforced := range []bool{false, true} {
		d, owner, ctl := ownerDevice(t)
		d.reopen(enforced)
		site := newTestCtl(t, fmt.Sprintf("site%v", enforced))
		if _, err := d.a.IssueDisplayGrant("", displayReq(site, "https://a.example", true)); !errors.Is(err, ErrNotOwner) {
			t.Fatalf("no approver outside legacy: %v", err)
		}
		if _, err := d.a.IssueDisplayGrant(ctl.ctl, displayReq(site, "https://a.example", true)); !errors.Is(err, ErrNotOwner) {
			t.Fatalf("controller approver: %v", err)
		}
		g := d.issue(owner.ctl, site, "https://a.example")
		if g.ApprovedBy != owner.ctl {
			t.Fatalf("approvedBy = %q", g.ApprovedBy)
		}
		// The file is separate from grants.json and survives a reload.
		if !strings.Contains(d.displayFile(), site.ctl) || strings.Contains(string(mustRead(t, d, grantsFile)), site.ctl) {
			t.Fatal("display grant not in its own file")
		}
		d.reopen(enforced)
		if _, ok := d.a.Store().DisplayGrant(site.ctl); !ok {
			t.Fatal("display grant lost on reload")
		}
	}
}

func mustRead(t *testing.T, d *testDevice, name string) []byte {
	t.Helper()
	b, err := d.fs.ReadFile(d.dir + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDisplayGrantCapsAndOnePerOrigin(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	for i := 0; i < maxActiveDisplayGrants; i++ {
		d.issue(owner.ctl, newTestCtl(t, fmt.Sprintf("s%d", i)), fmt.Sprintf("https://s%d.example", i))
	}
	if _, err := d.a.IssueDisplayGrant(owner.ctl, displayReq(newTestCtl(t, "over"), "https://over.example", true)); !errors.Is(err, ErrTooManyDisplayGrants) {
		t.Fatalf("over the cap: %v", err)
	}
	// A re-pair of an existing origin fits: it revokes the earlier grant.
	again := d.issue(owner.ctl, newTestCtl(t, "s0b"), "https://s0.example")
	n := 0
	for _, g := range d.a.DisplayGrants() {
		if g.Origin == "https://s0.example" && g.Active(testNow) {
			n++
			if g.Ctl != again.Ctl {
				t.Fatal("old grant still active")
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d active grants for one origin", n)
	}
	// An asserted origin never evicts an attested one (and vice versa).
	if _, err := d.a.IssueDisplayGrant(owner.ctl, displayReq(newTestCtl(t, "s0c"), "https://s0.example", false)); !errors.Is(err, ErrTooManyDisplayGrants) {
		t.Fatalf("asserted origin evicted an attested grant: %v", err)
	}
	if g, _ := d.a.Store().DisplayGrant(again.Ctl); !g.Active(testNow) {
		t.Fatal("attested grant revoked by a refused issuance")
	}
}

func TestDisplayGrantCtlCollisions(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	// A grants.json ctl cannot become a display grant.
	if _, err := d.a.IssueDisplayGrant(owner.ctl, displayReq(ctl, "https://a.example", true)); !errors.Is(err, ErrDisplayCollision) {
		t.Fatalf("grants.json ctl: %v", err)
	}
	site := newTestCtl(t, "site")
	d.issue(owner.ctl, site, "https://a.example")
	if _, err := d.a.IssueDisplayGrant(owner.ctl, displayReq(site, "https://a.example", true)); !errors.Is(err, ErrDisplayCollision) {
		t.Fatalf("display ctl again: %v", err)
	}
	// A display ctl cannot be registered or approved into grants.json.
	reg := `{"sig":` + string(mustJSON(site.sigPub)) + `,"enc":` + string(mustJSON(site.encPub)) + `,"label":"x","platform":"cli","role":"controller"}`
	r := d.expectCode(owner, d.cmdClaims(owner, CmdRegisterController, reg), CodeCommandFailed)
	if !strings.Contains(string(r.Res), "invalid_request") {
		t.Fatalf("register res = %s", r.Res)
	}
	r = d.expectCode(owner, d.cmdClaims(owner, CmdApproveController, `{"ctl":"`+site.ctl+`","role":"controller"}`), CodeCommandFailed)
	if !strings.Contains(string(r.Res), "not_pending") {
		t.Fatalf("approve res = %s", r.Res)
	}
	// Nor can it enroll or ask for access: every non-cmd op is refused.
	for _, op := range []Op{OpEnroll, OpRequestAccess} {
		rc := d.claims(site, op)
		rc.Enroll = site.enroll("")
		d.expectCode(site, rc, CodePermissionDenied)
	}
	if _, ok := d.a.Store().Grant(site.ctl); ok {
		t.Fatal("display ctl entered grants.json")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestDisplayGrantAdmission(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	site := newTestCtl(t, "site")
	d.issue(owner.ctl, site, "https://a.example")
	adm := d.expectExecute(site, d.cmdClaims(site, "displayPlaylist", testCast))
	if adm.Principal != (Principal{ControllerID: site.ctl, Role: RoleDisplay}) || adm.SignerRole != RoleDisplay {
		t.Fatalf("principal = %+v", adm.Principal)
	}
	// The window lives in the display store, and a replay is refused.
	if g, _ := d.a.Store().DisplayGrant(site.ctl); g.Window.Hi != 1 {
		t.Fatalf("window = %+v", g.Window)
	}
	rc := d.cmdClaims(site, "displayPlaylist", testCast)
	rc.Ctr = 1
	d.expectCode(site, rc, CodeReplay)
	// Denials spend the ctr in the display store too.
	d.expectCode(site, d.cmdClaims(site, "getDeviceStatus", `{}`), CodePermissionDenied)
	d.expectCode(site, d.cmdClaims(site, "displayPlaylist", `{"dp1_call":{}}`), CodePermissionDenied)
	d.expectCode(site, d.cmdClaims(site, CmdRevokeController, `{"ctl":"`+owner.ctl+`"}`), CodePermissionDenied)
	if g, _ := d.a.Store().DisplayGrant(site.ctl); g.Window.Hi != site.ctr {
		t.Fatalf("denials did not spend ctr: hi %d, ctr %d", g.Window.Hi, site.ctr)
	}
	// Display peers are never bound or notified.
	if d.a.GrantActive(site.ctl) {
		t.Fatal("GrantActive sees a display grant")
	}
	if _, err := d.a.Seal(site.ctl, "device_status", nil); err == nil {
		t.Fatal("Seal sealed to a display grant")
	}
	if _, err := d.a.SealOwner(site.ctl, "mint_pairing_approval_request", nil); err == nil {
		t.Fatal("SealOwner sealed to a display grant")
	}
	all, _ := d.a.SealAll("device_status", nil)
	for _, s := range all {
		if s.Ctl == site.ctl {
			t.Fatal("SealAll sealed to a display grant")
		}
	}
	// Self-revoke: the one control command, written to the display store.
	writes := d.fs.Writes
	grants := string(mustRead(t, d, grantsFile))
	rc = d.cmdClaims(site, CmdRevokeController, `{"ctl":"`+site.ctl+`"}`)
	adm = d.send(site, rc)
	if r := d.final(adm, rc); !r.OK || len(adm.RevokedCtls) != 1 || adm.RevokedCtls[0] != site.ctl {
		t.Fatalf("self-revoke: %+v", r)
	}
	if d.fs.Writes != writes+1 || string(mustRead(t, d, grantsFile)) != grants {
		t.Fatal("self-revoke wrote grants.json")
	}
	if g, _ := d.a.Store().DisplayGrant(site.ctl); g.RevokedAt != testNow || g.Window.Hi != site.ctr {
		t.Fatalf("after self-revoke: %+v", g)
	}
	d.expectCode(site, d.cmdClaims(site, "displayPlaylist", testCast), CodeAccessRevoked)
	// Inactive wins over what was asked: a non-cmd op from a revoked
	// display ctl is access_revoked too.
	rc = d.claims(site, OpEnroll)
	rc.Enroll = site.enroll("")
	d.expectCode(site, rc, CodeAccessRevoked)
}

func TestDisplayGrantInLegacyAndLocked(t *testing.T) {
	d := newTestDevice(t)
	site := newTestCtl(t, "site")
	d.issue("", site, "https://a.example")
	d.expectExecute(site, d.cmdClaims(site, "displayPlaylist", testCast))
	// Still valid once an owner enrolls.
	inv, _ := d.a.IssueInvitation()
	phone := newTestCtl(t, "phone")
	rc := d.claims(phone, OpEnroll)
	rc.Enroll = phone.enroll(inv.Value)
	d.expectOK(phone, rc)
	d.expectExecute(site, d.cmdClaims(site, "displayPlaylist", testCast))
	// Locked refuses it publicly like everything else.
	d.fs.SetFile(d.dir+"/grants.json", []byte(`{"v":1,"gen":"x","controllers":[]}`))
	a, _ := New(Config{Dir: d.dir, DeviceID: testDeviceID, FS: d.fs})
	if _, err := a.Open(d.seal(site, d.cmdClaims(site, "displayPlaylist", testCast))); publicCode(err) != CodeLocked {
		t.Fatalf("locked: %v", err)
	}
	if _, err := a.IssueDisplayGrant("", displayReq(newTestCtl(t, "x"), "https://b.example", true)); !errors.Is(err, ErrLocked) {
		t.Fatalf("issue while locked: %v", err)
	}
}

func TestOwnerRevokesDisplayGrantAndLists(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	site := newTestCtl(t, "site")
	d.issue(owner.ctl, site, "https://a.example")
	r := d.expectOK(owner, d.cmdClaims(owner, CmdListControllers, `{}`))
	var res struct {
		Controllers []map[string]any `json:"controllers"`
	}
	_ = json.Unmarshal(r.Res, &res)
	last := res.Controllers[len(res.Controllers)-1]
	if last["ctl"] != site.ctl || last["role"] != "display" || last["origin"] != "https://a.example" ||
		last["originAttested"] != true || last["platform"] != "web" || last["label"] != "a.example" {
		t.Fatalf("display row = %v", last)
	}
	// Two writes: the owner's window in grants.json, then the display store.
	writes := d.fs.Writes
	d.expectOK(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+site.ctl+`"}`))
	if d.fs.Writes != writes+2 {
		t.Fatalf("%d writes", d.fs.Writes-writes)
	}
	if g, _ := d.a.Store().DisplayGrant(site.ctl); g.RevokedAt != testNow {
		t.Fatal("not revoked")
	}
	r = d.expectCode(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+site.ctl+`"}`), CodeCommandFailed)
	if !strings.Contains(string(r.Res), "not_found") {
		t.Fatalf("second revoke: %s", r.Res)
	}
	// A failed second write is internal_error, with the owner's window spent.
	site2 := newTestCtl(t, "site2")
	d.issue(owner.ctl, site2, "https://b.example")
	d.fs.FailWrite = func(name string) error {
		if strings.HasSuffix(name, displayGrantsFile) {
			return errors.New("enospc")
		}
		return nil
	}
	d.expectCode(owner, d.cmdClaims(owner, CmdRevokeController, `{"ctl":"`+site2.ctl+`"}`), CodeInternalError)
	d.fs.FailWrite = nil
	if g, _ := d.a.Store().DisplayGrant(site2.ctl); g.RevokedAt != 0 {
		t.Fatal("revoked despite the failed write")
	}
	if g, _ := d.a.Store().Grant(owner.ctl); g.Window.Hi != owner.ctr {
		t.Fatal("owner window not spent")
	}
}

func TestDisplayStoreFailureNeverLocks(t *testing.T) {
	// Another gen: records of a dead identity, discarded as an empty store.
	{
		d, owner, _ := ownerDevice(t)
		d.issue(owner.ctl, newTestCtl(t, "old"), "https://a.example")
		d.fs.SetFile(d.dir+"/"+displayGrantsFile, []byte(strings.Replace(d.displayFile(), d.id.gen, "AAAAAAAAAAAAAAAAAAAAAA", 1)))
		d.reopen(false)
		if err := d.a.Store().DisplayErr(); err != nil || len(d.a.DisplayGrants()) != 0 {
			t.Fatalf("gen mismatch: err %v, %d records", err, len(d.a.DisplayGrants()))
		}
		d.issue(owner.ctl, newTestCtl(t, "new"), "https://b.example")
		if n := len(d.a.DisplayGrants()); n != 1 {
			t.Fatalf("%d records after overwrite", n)
		}
	}
	for _, bad := range []string{`not json`, `{"v":2,"gen":"x","grants":[]}`} {
		d, owner, _ := ownerDevice(t)
		d.fs.SetFile(d.dir+"/"+displayGrantsFile, []byte(bad))
		d.reopen(false)
		if d.a.State() != StateEnrolled || d.a.LockErr() != nil {
			t.Fatalf("%s: state %s, lock %v", bad, d.a.State(), d.a.LockErr())
		}
		if err := d.a.Store().DisplayErr(); !errors.Is(err, ErrDisplayDisabled) {
			t.Fatalf("%s: display err %v", bad, err)
		}
		if _, err := d.a.IssueDisplayGrant(owner.ctl, displayReq(newTestCtl(t, "s"), "https://a.example", true)); !errors.Is(err, ErrDisplayDisabled) {
			t.Fatalf("%s: issue %v", bad, err)
		}
		// Owners keep working.
		d.expectExecute(owner, d.cmdClaims(owner, "getDeviceStatus", `{}`))
	}
	// A loader that knows nothing of the file (grants.json only) is also
	// unaffected: the file is never read by LoadStore's locking path.
	d, owner, _ := ownerDevice(t)
	d.issue(owner.ctl, newTestCtl(t, "site"), "https://a.example")
	s, err := LoadStore(d.fs, d.dir, testDeviceID)
	if err != nil || len(s.Grants()) != 2 {
		t.Fatalf("reload with display file: %v", err)
	}
}

func TestDisplayGrantDurabilityAndRevoke(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	site := newTestCtl(t, "site")
	d.fs.FailSync = func(string) error { return errors.New("eio") }
	g, err := d.a.IssueDisplayGrant(owner.ctl, displayReq(site, "https://a.example", true))
	if !errors.Is(err, ErrDurability) || g.Ctl != site.ctl {
		t.Fatalf("durability: %v %+v", err, g)
	}
	d.fs.FailSync = nil
	// Committed: it is in memory and on disk, and the caller revokes it.
	if _, ok := d.a.Store().DisplayGrant(site.ctl); !ok {
		t.Fatal("committed grant missing")
	}
	if err := d.a.RevokeDisplayGrant(site.ctl); err != nil {
		t.Fatal(err)
	}
	if err := d.a.RevokeDisplayGrant(site.ctl); err != nil {
		t.Fatalf("revoke twice: %v", err)
	}
	if err := d.a.RevokeDisplayGrant("nope"); !errors.Is(err, ErrDisplayNotFound) {
		t.Fatalf("revoke unknown: %v", err)
	}
	d.expectCode(site, d.cmdClaims(site, "displayPlaylist", testCast), CodeAccessRevoked)
	// Retire clears it with the rest.
	if err := d.a.Retire(); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.a.Store().DisplayGrant(site.ctl); ok {
		t.Fatal("display grant survived retire")
	}
}

func TestDisplayLabel(t *testing.T) {
	for in, want := range map[string]string{
		"https://artblocks.io":        "artblocks.io",
		"https://bücher.example:8443": "xn--bcher-kva.example",
		"http://localhost:3000":       "localhost",
		strings.Repeat("a", 80):       strings.Repeat("a", 64),
		"":                            displayLabelFallback,
		"https://" + strings.Repeat("a", 70) + ".example": strings.Repeat("a", 64),
	} {
		if got := displayLabel(in); got != want {
			t.Errorf("displayLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPermits(t *testing.T) {
	display := Principal{ControllerID: "d", Role: RoleDisplay}
	for _, tc := range []struct {
		p   Principal
		cmd string
		req string
		ok  bool
	}{
		{display, "displayPlaylist", testCast, true},
		{display, "displayPlaylist", `{"dp1_call":{}}`, false},
		{display, "displayPlaylist", `{"intent":{"action":"now_display"},"dp1_call":{},"x":1}`, false},
		{display, "getDeviceStatus", `{}`, false},
		{display, "connect", `{}`, false},
		{display, CmdListControllers, `{}`, false},
		{display, "ping", `{}`, false},
		{Principal{Role: RoleController}, "displayPlaylist", `{"dp1_call":{}}`, true},
		{Principal{Role: RoleController}, "sshAccess", `{}`, false},
		{Principal{Role: RoleOwner}, "sshAccess", `{}`, true},
		{Principal{Role: RoleOwner}, "ping", `{}`, false},
	} {
		if got := Permits(tc.p, tc.cmd, json.RawMessage(tc.req)); got != tc.ok {
			t.Errorf("Permits(%s, %s, %s) = %v", tc.p.Role, tc.cmd, tc.req, got)
		}
	}
	if RoleDisplay.valid() || TierController.Allows(RoleDisplay) || TierOwner.Allows(RoleDisplay) {
		t.Fatal("display is wire-assignable or tiered")
	}
}
