package control

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenStoreCreatesOnlyWhenMissing(t *testing.T) {
	fsys := NewMemFS()
	s := OpenStore(fsys, "/s/control", "FF1-A", rand.NewChaCha8([32]byte{}), time.Unix(testNow, 0))
	if s.LockErr() != nil {
		t.Fatal(s.LockErr())
	}
	id := s.Identity()
	if id.DeviceID() != "FF1-A" || len(id.Gen()) != 22 {
		t.Fatalf("identity = %+v", id)
	}
	// Reopening loads the same identity.
	s2 := OpenStore(fsys, "/s/control", "FF1-A", rand.NewChaCha8([32]byte{9}), time.Unix(testNow, 0))
	if s2.LockErr() != nil || s2.Identity().Profile() != id.Profile() {
		t.Fatal("identity changed on reopen")
	}
	if _, err := fsys.Stat("/s/control.new"); err == nil {
		t.Fatal("staging dir left behind")
	}
	// A leftover staging dir from a crash does not block creation.
	fsys2 := NewMemFS()
	fsys2.SetFile("/s/control.new/identity.json", []byte("partial"))
	if s := OpenStore(fsys2, "/s/control", "FF1-A", rand.NewChaCha8([32]byte{}), time.Now()); s.LockErr() != nil {
		t.Fatal(s.LockErr())
	}
}

func TestCorruptStoreLocks(t *testing.T) {
	owner := newTestCtl(t, "owner")
	good := func() *MemFS {
		fsys := NewMemFS()
		if err := InitDir(fsys, "/s/control", testIdentity(t), []Grant{owner.grant(RoleOwner)}); err != nil {
			t.Fatal(err)
		}
		return fsys
	}
	gb, _ := good().ReadFile("/s/control/grants.json")
	ib, _ := good().ReadFile("/s/control/identity.json")
	grants, identity := string(gb), string(ib)
	edit := func(file, from, to string) func(*MemFS) {
		return func(m *MemFS) {
			src := grants
			if file == identityFile {
				src = identity
			}
			if !strings.Contains(src, from) {
				t.Fatalf("edit %q not applicable", from)
			}
			m.SetFile("/s/control/"+file, []byte(strings.Replace(src, from, to, 1)))
		}
	}
	cases := map[string]func(*MemFS){
		"grants garbage":     func(m *MemFS) { m.SetFile("/s/control/grants.json", []byte("{")) },
		"grants missing":     func(m *MemFS) { _ = m.RemoveAll("/s/control/grants.json") },
		"identity missing":   func(m *MemFS) { _ = m.RemoveAll("/s/control/identity.json") },
		"identity garbage":   func(m *MemFS) { m.SetFile("/s/control/identity.json", []byte("null")) },
		"gen mismatch":       edit(grantsFile, `"gen":"`, `"gen":"A`),
		"version":            edit(grantsFile, `{"v":1`, `{"v":2`),
		"duplicate member":   edit(grantsFile, `"role":"owner"`, `"role":"owner","role":"owner"`),
		"unknown member":     edit(grantsFile, `"role":"owner"`, `"role":"owner","admin":true`),
		"bad role":           edit(grantsFile, `"role":"owner"`, `"role":"root"`),
		"ctl not thumb":      edit(grantsFile, `"ctl":"`+owner.ctl, `"ctl":"`+newTestCtl(t, "x").ctl),
		"bad window":         edit(grantsFile, `"bitmap":"0000000000000000"`, `"bitmap":"0000000000000002"`),
		"uppercase bitmap":   edit(grantsFile, `"hi":0,"bitmap":"0000000000000000"`, `"hi":1,"bitmap":"000000000000000A"`),
		"float hi":           edit(grantsFile, `"hi":0`, `"hi":0.5`),
		"short nk":           edit(grantsFile, `"nk":"`, `"nk":"AA`),
		"negative createdAt": edit(identityFile, `"createdAt":1`, `"createdAt":-1`),
		"identity extra":     edit(identityFile, `{"v":1`, `{"v":1,"dev":"x"`),
		"dir is a file":      func(m *MemFS) { _ = m.RemoveAll("/s/control"); m.SetFile("/s/control", []byte("x")) },
		"bad platform":       edit(grantsFile, `"platform":"cli"`, `"platform":"windows"`),
		"bad approvedBy":     edit(grantsFile, `"approvedBy":"invitation"`, `"approvedBy":"someone"`),
		"label too long":     edit(grantsFile, `"label":"owner"`, `"label":"`+strings.Repeat("x", 65)+`"`),
		"identity point off": edit(identityFile, `"x":"`, `"x":"A`),
		"owner expiresAt":    edit(grantsFile, `"role":"owner"`, `"role":"owner","expiresAt":4102444800`),
	}
	for name, corrupt := range cases {
		fsys := good()
		corrupt(fsys)
		s := OpenStore(fsys, "/s/control", testDeviceID, rand.NewChaCha8([32]byte{}), time.Now())
		if !errors.Is(s.LockErr(), ErrLocked) {
			t.Errorf("%s: LockErr = %v, want ErrLocked", name, s.LockErr())
			continue
		}
		// Locked never means "start over": the files are untouched.
		if b, _ := fsys.ReadFile("/s/control/identity.json"); name != "identity missing" && name != "dir is a file" && len(b) == 0 {
			t.Errorf("%s: identity rewritten", name)
		}
		if err := s.Update(func(g []Grant) ([]Grant, error) { return g, nil }); !errors.Is(err, ErrLocked) {
			t.Errorf("%s: Update on a locked store = %v", name, err)
		}
	}
}

func TestStoreUpdateSemantics(t *testing.T) {
	owner := newTestCtl(t, "owner")
	fsys := NewMemFS()
	_ = InitDir(fsys, "/s/c", testIdentity(t), []Grant{owner.grant(RoleOwner)})
	s, err := LoadStore(fsys, "/s/c", testDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	bump := func(gs []Grant) ([]Grant, error) {
		gs[0].Window = gs[0].Window.Accept(gs[0].Window.Hi + 1)
		return gs, nil
	}
	// fn error: nothing written.
	writes := fsys.Writes
	if err := s.Update(func([]Grant) ([]Grant, error) { return nil, errors.New("no") }); err == nil || fsys.Writes != writes {
		t.Fatal("fn error still wrote")
	}
	// Invalid result is refused before writing.
	if err := s.Update(func(gs []Grant) ([]Grant, error) { gs[0].Role = "x"; return gs, nil }); err == nil {
		t.Fatal("persisted an invalid record")
	}
	// Uncommitted write: memory unchanged.
	fsys.FailWrite = func(string) error { return errors.New("enospc") }
	if err := s.Update(bump); err == nil {
		t.Fatal("expected write failure")
	}
	if g, _ := s.Grant(owner.ctl); g.Window.Hi != 0 {
		t.Fatal("memory ran ahead of disk")
	}
	fsys.FailWrite = nil
	// Committed but unsynced: memory follows the file, error says so.
	fsys.FailSync = func(string) error { return errors.New("eio") }
	if err := s.Update(bump); !errors.Is(err, ErrDurability) {
		t.Fatalf("err = %v", err)
	}
	if g, _ := s.Grant(owner.ctl); g.Window.Hi != 1 {
		t.Fatal("memory disagrees with the committed file")
	}
	fsys.FailSync = nil
	if err := s.Update(bump); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadStore(fsys, "/s/c", testDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := s2.Grant(owner.ctl); g.Window != (Window{Hi: 2, Bitmap: 3}) {
		t.Fatalf("reloaded window = %+v", g.Window)
	}
}

func TestStoreConcurrentUpdates(t *testing.T) {
	owner := newTestCtl(t, "owner")
	fsys := NewMemFS()
	_ = InitDir(fsys, "/s/c", testIdentity(t), []Grant{owner.grant(RoleOwner)})
	s, _ := LoadStore(fsys, "/s/c", testDeviceID)
	const n = 64
	var wg sync.WaitGroup
	accepted := make([]bool, n+1)
	var mu sync.Mutex
	for i := 1; i <= n; i++ {
		for range 3 { // every ctr raced three times
			wg.Add(1)
			go func(ctr uint64) {
				defer wg.Done()
				err := s.Update(func(gs []Grant) ([]Grant, error) {
					if err := gs[0].Window.Check(ctr); err != nil {
						return nil, err
					}
					gs[0].Window = gs[0].Window.Accept(ctr)
					return gs, nil
				})
				if err == nil {
					mu.Lock()
					if accepted[ctr] {
						t.Errorf("ctr %d accepted twice", ctr)
					}
					accepted[ctr] = true
					mu.Unlock()
				}
				_ = s.Grants()
			}(uint64(i))
		}
	}
	wg.Wait()
	g, _ := s.Grant(owner.ctl)
	if g.Window.Hi != n {
		t.Fatalf("hi = %d", g.Window.Hi)
	}
}

func TestOSFSAtomicWriteAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "control")
	o := &OSFS{}
	if err := InitDir(o, dir, testIdentity(t), nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v %v", fi.Mode().Perm(), err)
	}
	for _, f := range []string{identityFile, grantsFile} {
		fi, err := os.Stat(filepath.Join(dir, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v %v", f, fi.Mode().Perm(), err)
		}
	}
	s, err := LoadStore(o, dir, testDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	before := o.Fsyncs()
	if err := s.Update(func(gs []Grant) ([]Grant, error) { return gs, nil }); err != nil {
		t.Fatal(err)
	}
	if got := o.Fsyncs() - before; got != 2 {
		t.Fatalf("fsyncs per update = %d, want 2 (file + dir)", got)
	}
	if _, err := os.Stat(filepath.Join(dir, grantsFile+".tmp")); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	if err := InitDir(o, dir, testIdentity(t), nil); err == nil {
		t.Fatal("InitDir replaced an existing dir")
	}
}

func TestIdentityRoundTrip(t *testing.T) {
	id := testIdentity(t)
	b, err := id.marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := parseIdentity(b, testDeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Profile() != id.Profile() || back.Pins() != id.Pins() {
		t.Fatal("identity changed in round trip")
	}
	p := id.Profile()
	if p.V != 1 || p.Dev != testDeviceID || p.Sig.Thumbprint() != id.Pins().Sig || p.Enc.Thumbprint() != id.Pins().Enc {
		t.Fatalf("profile = %+v", p)
	}
	if _, err := NewIdentityFromKeys("", id.gen, id.sig, id.enc, 0); err == nil {
		t.Fatal("empty device id accepted")
	}
	if _, err := NewIdentityFromKeys("x", "short", id.sig, id.enc, 0); err == nil {
		t.Fatal("bad gen accepted")
	}
}

func TestRetire(t *testing.T) {
	d, owner, _ := ownerDevice(t)
	if err := d.a.Retire(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.fs.Stat(d.dir); err == nil {
		t.Fatal("control dir survived Retire")
	}
	if _, err := d.fs.Stat(RetiredMarker(d.dir)); err != nil {
		t.Fatal("no retirement tombstone")
	}
	if d.a.State() != StateLocked || !errors.Is(d.a.LockErr(), ErrRetired) {
		t.Fatalf("state after retire = %s", d.a.State())
	}
	if publicCode(func() error { _, err := d.a.Open(d.seal(owner, d.cmdClaims(owner, "rotate", `{}`))); return err }()) != CodeLocked {
		t.Fatal("retired authority admitted a request")
	}
	// A restart on the same subvolume (the reset rolled back) stays retired:
	// the tombstone forces locked rather than "no state".
	a, _ := New(Config{Dir: d.dir, DeviceID: testDeviceID, FS: d.fs})
	if a.State() != StateLocked || !errors.Is(a.LockErr(), ErrRetired) || !StateExists(d.fs, d.dir) {
		t.Fatalf("restart after retire: %s %v", a.State(), a.LockErr())
	}
	// Retired accepts plaintext factoryReset (so the reset can be retried)
	// and nothing else owner-tier.
	if !a.Policy(RouteLAN, "factoryReset", nil).Allow || !a.Policy(RouteRelayer, "factoryReset", nil).Allow {
		t.Fatal("retired device refuses the reset retry")
	}
	if a.Policy(RouteLAN, "displayPlaylist", nil).Allow || a.Policy(RouteRelayer, "sshAccess", nil).Allow {
		t.Fatal("retired device opened other plaintext")
	}
	// Review L2: not even relayer controller-tier plaintext.
	if a.Policy(RouteRelayer, "rotate", nil).Allow || a.Policy(RouteRelayer, "displayPlaylist", nil).Allow {
		t.Fatal("retired device allowed relayer controller-tier plaintext")
	}
	// A completed reset boots a subvolume without the tombstone: legacy, new gen.
	_ = d.fs.RemoveAll(RetiredMarker(d.dir))
	a, _ = New(Config{Dir: d.dir, DeviceID: testDeviceID, FS: d.fs})
	if a.State() != StateLegacy || a.Store().Identity().Gen() == d.id.gen {
		t.Fatal("fresh subvolume did not start over")
	}
	// A failing delete is reported and still locks.
	d2, _, _ := ownerDevice(t)
	failing := &failRemoveFS{MemFS: d2.fs}
	s := OpenStore(failing, d2.dir, testDeviceID, nil, time.Now())
	if err := s.Retire(); err == nil || !errors.Is(s.LockErr(), ErrLocked) {
		t.Fatal("failed retire not reported or not locked")
	}
	// If the tombstone cannot be written nothing is deleted.
	d3, _, _ := ownerDevice(t)
	d3.fs.FailWrite = func(name string) error {
		if strings.HasSuffix(name, ".retired") {
			return errors.New("enospc")
		}
		return nil
	}
	if err := d3.a.Retire(); err == nil {
		t.Fatal("retire without a tombstone reported success")
	}
	// Review F5: nothing changed in memory either (nor the pending table).
	if d3.a.LockErr() != nil || d3.a.State() != StateEnrolled {
		t.Fatalf("failed retire changed memory: %v %s", d3.a.LockErr(), d3.a.State())
	}
	if _, err := d3.fs.Stat(d3.dir); err != nil {
		t.Fatal("control dir deleted although the tombstone was not written")
	}
}

func TestStateExists(t *testing.T) {
	fsys := NewMemFS()
	if StateExists(fsys, "/s/control") {
		t.Fatal("empty fs has state")
	}
	fsys.SetFile("/s/control.retired", []byte("{}"))
	if !StateExists(fsys, "/s/control") {
		t.Fatal("tombstone not counted")
	}
	fsys2 := NewMemFS()
	_ = InitDir(fsys2, "/s/control", testIdentity(t), nil)
	if !StateExists(fsys2, "/s/control") {
		t.Fatal("dir not counted")
	}
	fsys3 := NewMemFS()
	fsys3.SetFile("/s/control.new/identity.json", []byte("partial"))
	if !StateExists(fsys3, "/s/control") {
		t.Fatal("staging dir not counted (review F9)")
	}
	if !StateExists(statErrFS{fsys}, "/s/x") {
		t.Fatal("an unreadable answer must count as present")
	}
}

type statErrFS struct{ *MemFS }

func (statErrFS) Stat(string) (fs.FileInfo, error) { return nil, errors.New("eio") }

type failRemoveFS struct{ *MemFS }

func (failRemoveFS) RemoveAll(string) error { return errors.New("ebusy") }

func TestEnsureInvitation(t *testing.T) {
	d := newTestDevice(t)
	a, err := d.a.EnsureInvitation(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := d.a.EnsureInvitation(time.Minute)
	if a.Value != b.Value {
		t.Fatal("live invitation reissued")
	}
	d.advance(invitationTTL - 30)
	c, _ := d.a.EnsureInvitation(time.Minute)
	if c.Value == a.Value || c.ExpiresAt <= a.ExpiresAt {
		t.Fatal("nearly spent invitation not refreshed")
	}
	d2, _, _ := ownerDevice(t)
	if _, err := d2.a.EnsureInvitation(time.Minute); err == nil {
		t.Fatal("invitation issued on an enrolled device")
	}
}

// Review F5: an unreadable tombstone is locked, but not "retired": the
// plaintext factoryReset allowance is only for a real tombstone.
func TestUnreadableTombstoneIsPlainLocked(t *testing.T) {
	d, _, _ := ownerDevice(t)
	fsys := markerErrFS{d.fs}
	a, _ := New(Config{Dir: d.dir, DeviceID: testDeviceID, FS: fsys})
	if a.State() != StateLocked || errors.Is(a.LockErr(), ErrRetired) || !errors.Is(a.LockErr(), ErrLocked) {
		t.Fatalf("state %s err %v", a.State(), a.LockErr())
	}
	if a.Policy(RouteLAN, "factoryReset", nil).Allow {
		t.Fatal("unreadable tombstone allowed the plaintext reset")
	}
}

type markerErrFS struct{ *MemFS }

func (m markerErrFS) Stat(name string) (fs.FileInfo, error) {
	if strings.HasSuffix(name, ".retired") {
		return nil, errors.New("eio")
	}
	return m.MemFS.Stat(name)
}

// Review nit: a failed Retire (no tombstone) leaves the invitation and the
// pending table as they were.
func TestFailedRetireKeepsInvitation(t *testing.T) {
	d := newTestDevice(t)
	inv, _ := d.a.IssueInvitation()
	d.fs.FailWrite = func(name string) error {
		if strings.HasSuffix(name, ".retired") {
			return errors.New("enospc")
		}
		return nil
	}
	if err := d.a.Retire(); err == nil {
		t.Fatal("retire without tombstone succeeded")
	}
	if cur, ok := d.a.CurrentInvitation(); !ok || cur.Value != inv.Value {
		t.Fatal("failed retire voided the invitation")
	}
}
