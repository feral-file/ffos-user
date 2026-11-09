package contentpath

import "testing"

const txA = "EwKI3znILCW86MZiIwji_kxrFG9Rdp-2arLMpxjyojs"

func TestIPFS(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, cid, rest string
		ok                  bool
	}{
		{name: "bare cid", in: "/ipfs/" + cidV0, cid: cidV0, ok: true},
		{name: "trailing slash is kept", in: "/ipfs/" + cidV0 + "/", cid: cidV0, rest: "/", ok: true},
		{name: "remainder is kept", in: "/ipfs/" + cidV1B32 + "/frames/001.png", cid: cidV1B32, rest: "/frames/001.png", ok: true},
		{name: "percent-encoding in the remainder is kept", in: "/ipfs/" + cidV1B32 + "/a%20b.png", cid: cidV1B32, rest: "/a%20b.png", ok: true},
		{name: "double slash before the cid", in: "/ipfs//" + cidV0 + "/17.png", cid: cidV0, rest: "/17.png", ok: true},
		{name: "slash runs everywhere before the cid", in: "//ipfs///" + cidV0, cid: cidV0, ok: true},
		{name: "doubled ipfs prefix", in: "/ipfs/ipfs/" + cidV0, cid: cidV0, ok: true},
		{name: "tripled ipfs prefix with slashes", in: "/ipfs/ipfs//ipfs/" + cidV1B32 + "/x.png", cid: cidV1B32, rest: "/x.png", ok: true},
		{name: "slash run inside the remainder is not ours", in: "/ipfs/" + cidV0 + "//a//b", cid: cidV0, rest: "//a//b", ok: true},
		{name: "bare prefix", in: "/ipfs/"},
		{name: "prefix without slash", in: "/ipfs"},
		{name: "empty", in: ""},
		{name: "doubled prefix without a cid", in: "/ipfs/ipfs/notacid"},
		{name: "ipns", in: "/ipns/example.com/x.png"},
		{name: "api path", in: "/api/v0/cat"},
		{name: "prefix must be exactly ipfs", in: "/ipfsx/" + cidV0},
		{name: "percent-encoded byte in the cid", in: "/ipfs/" + cidV0[:45] + "%2F"},
		{name: "cid with a trailing character", in: "/ipfs/" + cidV0 + "x"},
		{name: "ipfs segment after the cid is the remainder", in: "/ipfs/" + cidV0 + "/ipfs/" + cidV1B32, cid: cidV0, rest: "/ipfs/" + cidV1B32, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cid, rest, ok := IPFS(tt.in)
			if ok != tt.ok || cid != tt.cid || rest != tt.rest {
				t.Fatalf("IPFS(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.in, cid, rest, ok, tt.cid, tt.rest, tt.ok)
			}
		})
	}
}

func TestArweave(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, tx, rest string
		ok                 bool
	}{
		{name: "bare tx", in: "/" + txA, tx: txA, ok: true},
		{name: "trailing slash is kept", in: "/" + txA + "/", tx: txA, rest: "/", ok: true},
		{name: "remainder is kept", in: "/" + txA + "/index.html", tx: txA, rest: "/index.html", ok: true},
		{name: "double slash before the tx", in: "//" + txA + "/index.html", tx: txA, rest: "/index.html", ok: true},
		{name: "root", in: "/"},
		{name: "empty", in: ""},
		{name: "not a tx", in: "/not-a-tx"},
		{name: "too long", in: "/" + txA + "x"},
		{name: "wrong alphabet", in: "/" + txA[:42] + "+"},
		{name: "a cid is not a tx", in: "/ipfs/" + cidV0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx, rest, ok := Arweave(tt.in)
			if ok != tt.ok || tx != tt.tx || rest != tt.rest {
				t.Fatalf("Arweave(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.in, tx, rest, ok, tt.tx, tt.rest, tt.ok)
			}
		})
	}
}
