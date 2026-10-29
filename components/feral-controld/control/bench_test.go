package control

import (
	"crypto/sha256"
	"encoding/binary"
	"math/rand/v2"
	"testing"
	"time"
)

// BenchmarkOpenPersist measures one admitted op=cmd end to end on the real
// filesystem: ECDH + Concat KDF + AES-GCM, ES256 verify, claims, the
// replay-window write (temp file, fsync, rename, dir fsync) and the signed,
// encrypted reply. fsyncs/op is reported so the durability cost is visible.
func BenchmarkOpenPersist(b *testing.B) {
	benchOpen(b, &OSFS{}, b.TempDir()+"/control")
}

// BenchmarkOpenMemFS is the same path with no disk: the crypto-only cost.
func BenchmarkOpenMemFS(b *testing.B) {
	benchOpen(b, NewMemFS(), "/s/control")
}

func benchOpen(b *testing.B, fsys FS, dir string) {
	owner := newTestCtl(b, "owner")
	id := testIdentity(b)
	if err := InitDir(fsys, dir, id, []Grant{owner.grant(RoleOwner)}); err != nil {
		b.Fatal(err)
	}
	a, err := New(Config{Dir: dir, DeviceID: testDeviceID, FS: fsys,
		Now: func() time.Time { return time.Unix(testNow, 0) }, Rand: rand.NewChaCha8([32]byte{})})
	if err != nil || a.LockErr() != nil {
		b.Fatal(err, a.LockErr())
	}
	compacts := make([]string, b.N)
	for i := range compacts {
		var rk [32]byte
		binary.BigEndian.PutUint64(rk[:], uint64(i))
		rk = sha256.Sum256(rk[:])
		rc := RequestClaims{V: 1, Dev: testDeviceID, Gen: id.gen, Ctl: owner.ctl, Rid: "bench", Ctr: uint64(i + 1),
			Iat: testNow, Exp: testNow + 60, Op: OpCmd, RK: b64u(rk[:]), Cmd: "displayPlaylist", Req: []byte(`{"dp1_call":{}}`)}
		payload, _ := rc.Marshal()
		compacts[i] = sealPayload(b, id, owner.sig, payload)
	}
	var startFsyncs int64
	osfs, isOS := fsys.(*OSFS)
	if isOS {
		startFsyncs = osfs.Fsyncs()
	}
	b.ResetTimer()
	for i := range b.N {
		adm, err := a.Open(compacts[i])
		if err != nil || !adm.Execute() {
			b.Fatalf("not admitted: %v", err)
		}
		if _, err := adm.Reply(true, "", map[string]bool{"ok": true}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if isOS {
		b.ReportMetric(float64(osfs.Fsyncs()-startFsyncs)/float64(b.N), "fsyncs/op")
	}
}
