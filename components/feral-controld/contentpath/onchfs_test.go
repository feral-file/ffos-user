package contentpath

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// caseyReas is the root of Casey Reas, There's No Distance #1 (eip155:1), the
// work ffos-user#427 was filed for.
const caseyReas = "1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a"

func TestParseOnchfs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Onchfs
	}{
		{"indexer form with fxhash seed", "onchfs://" + caseyReas + "/?fxhash=0x06ec&fxiteration=1&fxminter=0xabc",
			Onchfs{Root: caseyReas, Query: "fxhash=0x06ec&fxiteration=1&fxminter=0xabc", HasQuery: true}},
		{"bare root", "onchfs://" + caseyReas, Onchfs{Root: caseyReas}},
		{"query without slash", "onchfs://" + caseyReas + "?fxhash=1", Onchfs{Root: caseyReas, Query: "fxhash=1", HasQuery: true}},
		{"upper-case scheme and root", "ONCHFS://" + "1639E8C989CDDD2C0F9E951B126A21B1964309854C0A8E0A9B1193A655C6712A/sketch.js",
			Onchfs{Root: caseyReas, Path: "sketch.js"}},
		{"nested path, empty query, fragment", "onchfs://" + caseyReas + "/lib/p5.js?#top",
			Onchfs{Root: caseyReas, Path: "lib/p5.js", HasQuery: true, Fragment: "top", HasFragment: true}},
		{"directory path", "onchfs://" + caseyReas + "/assets/", Onchfs{Root: caseyReas, Path: "assets/"}},
		{"escaped name", "onchfs://" + caseyReas + "/my%20file.js", Onchfs{Root: caseyReas, Path: "my%20file.js"}},
		{"ethereum authority", "onchfs://eip155:1/" + caseyReas, Onchfs{Root: caseyReas, Chain: "eip155:1"}},
		{"namespace alone is mainnet", "onchfs://eip155/" + caseyReas, Onchfs{Root: caseyReas, Chain: "eip155:1"}},
		{"base authority", "onchfs://eip155:8453/" + caseyReas, Onchfs{Root: caseyReas, Chain: "eip155:8453"}},
		{"tezos alias", "onchfs://tezos:mainnet/" + caseyReas, Onchfs{Root: caseyReas, Chain: "tezos:NetXdQprcVkpaWU"}},
		{"default contract", "onchfs://eip155:1:0x9E0F2864C6F125BBF599DF6CA6E6C3774C5B2E04/" + caseyReas,
			Onchfs{Root: caseyReas, Chain: "eip155:1"}},
		{"tezos default contract", "onchfs://tezos:NetXdQprcVkpaWU:KT1Ae7dT1gsLw2tRnUMXSCmEyF74KVkM6LUo/" + caseyReas,
			Onchfs{Root: caseyReas, Chain: "tezos:NetXdQprcVkpaWU"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseOnchfs(tc.in)
			require.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}

	for _, bad := range []string{
		"",
		"https://" + caseyReas,
		"onchfs://",
		"onchfs://abc",
		"onchfs://" + caseyReas[:63] + "g",
		"onchfs://" + caseyReas + "/../x",
		"onchfs://" + caseyReas + "/a/%2e%2e/b",
		"onchfs://" + caseyReas + "/a//b",
		"onchfs://" + caseyReas + `/a\b`,
		"onchfs://eip155:5/" + caseyReas, // testnet
		"onchfs://eip155:1:0x68b75b4e8439a7099e53045bea850b3266e95906/" + caseyReas, // another contract
		"onchfs://solana/" + caseyReas,
		"onchfs://eip155:1",
	} {
		_, ok := ParseOnchfs(bad)
		assert.False(t, ok, bad)
	}
}

func TestOnchfsSourceURLRoundTrip(t *testing.T) {
	cases := []struct {
		in, source string
	}{
		{"onchfs://" + caseyReas + "/?fxhash=0x06ec&fxiteration=1",
			"https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.onchfs.invalid/?fxhash=0x06ec&fxiteration=1"},
		// the bare root and the slashed root are one work and one cache key
		{"onchfs://" + caseyReas + "?fxhash=0x06ec&fxiteration=1",
			"https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.onchfs.invalid/?fxhash=0x06ec&fxiteration=1"},
		{"onchfs://" + caseyReas, "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.onchfs.invalid/"},
		{"onchfs://eip155:1/" + caseyReas + "/sketch.js#x",
			"https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.eth.onchfs.invalid/sketch.js#x"},
		{"onchfs://tezos/" + caseyReas + "/a%20b/?",
			"https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.tezos.onchfs.invalid/a%20b/?"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			o, ok := ParseOnchfs(tc.in)
			require.True(t, ok)
			src := o.SourceURL()
			assert.Equal(t, tc.source, src)
			back, ok := ParseOnchfsSourceURL(src)
			require.True(t, ok)
			assert.Equal(t, o, back)
			assert.Equal(t, src, back.SourceURL())
			assert.True(t, IsOnchfsSourceURL(src))
		})
	}
}

func TestOnchfsURI(t *testing.T) {
	o, ok := ParseOnchfs("ONCHFS://eip155/" + caseyReas + "?fxhash=1")
	require.True(t, ok)
	assert.Equal(t, "onchfs://eip155:1/"+caseyReas+"/?fxhash=1", o.URI())
}

func TestParseOnchfsSourceURLRefusesLookAlikes(t *testing.T) {
	host := "cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva"
	for _, bad := range []string{
		"http://" + host + ".onchfs.invalid/",               // not https
		"https://" + host + ".onchfs.invalid:443/",          // explicit port
		"https://u@" + host + ".onchfs.invalid/",            // userinfo
		"https://" + host + ".onchfs.invalid.example/",      // suffix look-alike
		"https://" + host + "x.onchfs.invalid/",             // 53 characters
		"https://" + host[:51] + ".onchfs.invalid/",         // 51 characters
		"https://" + host + ".goerli.onchfs.invalid/",       // unknown chain label
		"https://" + host + ".eth.eth.onchfs.invalid/",      // extra label
		"https://" + host[:51] + "1.onchfs.invalid/",        // not base32
		"https://" + host + ".onchfs.invalid/../x",          // climbs out
		"https://evil.example/" + host + ".onchfs.invalid/", // host elsewhere
		"https://onchfs.invalid/",
	} {
		assert.False(t, IsOnchfsSourceURL(bad), bad)
	}
	// host case does not matter: Chromium lower-cases it anyway
	o, ok := ParseOnchfsSourceURL("https://" + "CY46RSMJZXOSYD46SUNRE2RBWGLEGCMFJQFI4CU3CGJ2MVOGOEVA.ONCHFS.INVALID/")
	require.True(t, ok)
	assert.Equal(t, caseyReas, o.Root)
}
