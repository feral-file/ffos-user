package control

import (
	"errors"
	"strings"
	"testing"
)

func TestLANSwitchDefaultsOpenAndPersists(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	if !d.a.LANOpen() || d.a.Status().LAN != LANOpen {
		t.Fatalf("fresh device: open %v, status %q", d.a.LANOpen(), d.a.Status().LAN)
	}
	r := d.expectOK(owner, d.cmdClaims(owner, CmdSetLanControl, `{"open":false}`))
	if string(r.Res) != `{"open":false}` {
		t.Fatalf("res = %s", r.Res)
	}
	d.reopen()
	if d.a.LANOpen() || d.a.Status().LAN != LANClosed {
		t.Fatalf("after restart: open %v, status %q", d.a.LANOpen(), d.a.Status().LAN)
	}
	if raw, _ := d.fs.ReadFile(d.dir + "/" + lanFile); !strings.Contains(string(raw), `"gen":"`+d.id.gen+`"`) {
		t.Fatalf("lan.json = %s", raw)
	}
	d.expectOK(owner, d.cmdClaims(owner, CmdSetLanControl, `{"open":true}`))
	d.reopen()
	if !d.a.LANOpen() {
		t.Fatal("reopened switch did not persist")
	}
}

func TestLANSwitchOwnerOnlyAndStrict(t *testing.T) {
	d, owner, ctl := ownerDevice(t)
	d.expectCode(ctl, d.cmdClaims(ctl, CmdSetLanControl, `{"open":false}`), CodePermissionDenied)
	for _, bad := range []string{`{}`, `{"open":"false"}`, `{"open":false,"x":1}`} {
		d.expectCode(owner, d.cmdClaims(owner, CmdSetLanControl, bad), CodeCommandFailed)
	}
	if !d.a.LANOpen() {
		t.Fatal("refused requests changed the switch")
	}
}

func TestLANSwitchFileFailureDirection(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		open, err  bool
	}{
		{"gen mismatch reads open", `{"v":1,"gen":"AAAAAAAAAAAAAAAAAAAAAA","open":false}`, true, false},
		{"corrupt reads closed", `not json`, false, true},
		{"unknown member reads closed", `{"v":1,"gen":"GEN","open":true,"x":1}`, false, true},
		{"other version reads closed", `{"v":2,"gen":"GEN","open":true}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, owner, _ := ownerDevice(t)
			d.fs.SetFile(d.dir+"/"+lanFile, []byte(strings.ReplaceAll(tc.body, "GEN", d.id.gen)))
			d.reopen()
			// Never locks: the switch is not part of grants.json.
			if d.a.State() != StateEnrolled {
				t.Fatalf("state %s", d.a.State())
			}
			if d.a.LANOpen() != tc.open || (d.a.Store().LANErr() != nil) != tc.err {
				t.Fatalf("open %v err %v", d.a.LANOpen(), d.a.Store().LANErr())
			}
			// The owner can always rewrite it.
			d.expectOK(owner, d.cmdClaims(owner, CmdSetLanControl, `{"open":true}`))
			if !d.a.LANOpen() || d.a.Store().LANErr() != nil {
				t.Fatal("rewrite did not recover the switch")
			}
		})
	}
}

func TestLANSwitchWriteFailureLeavesSwitch(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	d.fs.FailWrite = func(name string) error {
		if strings.HasSuffix(name, lanFile) {
			return errors.New("disk full")
		}
		return nil
	}
	d.expectCode(owner, d.cmdClaims(owner, CmdSetLanControl, `{"open":false}`), CodeInternalError)
	if !d.a.LANOpen() {
		t.Fatal("memory ran ahead of a failed write")
	}
}

func TestStatusLANMemberOnlyWhenOwned(t *testing.T) {
	d := newTestDevice(t)
	if st := d.a.Status(); st.State != StateLegacy || st.LAN != "" {
		t.Fatalf("legacy status %+v", st)
	}
	d, _, _ = ownerDevice(t)
	if st := d.a.Status(); st.LAN != LANOpen {
		t.Fatalf("enrolled status %+v", st)
	}
}
