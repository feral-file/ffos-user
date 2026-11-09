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

func TestSlashBeforeQuery(t *testing.T) {
	t.Parallel()
	host := "https://ipfs.raribleuserdata.com"
	tests := []struct {
		name, in, want string
		ok             bool
	}{
		{name: "bare cid with query", in: host + "/ipfs/" + cidV0 + "?fxhash=abc", want: host + "/ipfs/" + cidV0 + "/?fxhash=abc", ok: true},
		{name: "query and fragment kept", in: host + "/ipfs/" + cidV1B32 + "?a=1&b=2#frag", want: host + "/ipfs/" + cidV1B32 + "/?a=1&b=2#frag", ok: true},
		{name: "slash run before the cid is normalized", in: host + "/ipfs//" + cidV0 + "?x=1", want: host + "/ipfs/" + cidV0 + "/?x=1", ok: true},
		{name: "any host", in: "http://localhost:8080/ipfs/" + cidV0 + "?x=1", want: "http://localhost:8080/ipfs/" + cidV0 + "/?x=1", ok: true},
		{name: "empty query with a question mark", in: host + "/ipfs/" + cidV0 + "?", want: host + "/ipfs/" + cidV0 + "/?", ok: true},
		{name: "already slashed is a no-op", in: host + "/ipfs/" + cidV0 + "/?fxhash=abc"},
		{name: "no query", in: host + "/ipfs/" + cidV0},
		{name: "a file under the cid", in: host + "/ipfs/" + cidV0 + "/index.html?x=1"},
		{name: "not a cid", in: host + "/ipfs/notacid?x=1"},
		{name: "ipns", in: host + "/ipns/example.com?x=1"},
		{name: "arweave is not touched", in: "https://arweave.net/" + txA + "?x=1"},
		{name: "no host", in: "data:text/plain,hi?x=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := SlashBeforeQuery(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("SlashBeforeQuery(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}
