package web3_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/web3"
)

// fixture is testdata/digital-soul.json: one eth_call recorded from chain on
// 2026-11-24 (Han Saglam, Digital Soul, artpiece() on Ethereum). The reader
// is tested against what a node actually answered, as the onchfs reader is
// (ffos-user#428).
type fixture struct {
	URI           string `json:"uri"`
	Contract      string `json:"contract"`
	ChainID       uint64 `json:"chainId"`
	Calldata      string `json:"calldata"`
	EthCallResult string `json:"ethCallResult"`
	BodyBytes     int    `json:"bodyBytes"`
	BodySha256    string `json:"bodySha256"`
	BodyPrefix    string `json:"bodyPrefix"`
	BodySuffix    string `json:"bodySuffix"`
}

func loadFixture(t *testing.T) (fixture, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/digital-soul.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decoding fixture: %v", err)
	}
	answer, err := hex.DecodeString(strings.TrimPrefix(f.EthCallResult, "0x"))
	if err != nil {
		t.Fatalf("decoding the recorded answer: %v", err)
	}
	return f, answer
}

func TestPlanBuildsTheRecordedCalldata(t *testing.T) {
	f, _ := loadFixture(t)
	p, err := web3.NewPlan(web3.Request{Contract: f.Contract, ChainID: f.ChainID, Path: "artpiece"}, web3.ModeAuto)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if got := "0x" + hex.EncodeToString(p.Calldata); got != f.Calldata {
		t.Fatalf("calldata = %s, want %s (the call the fixture was recorded with)", got, f.Calldata)
	}
	if p.ContentType != "text/html" {
		t.Fatalf("ContentType = %q, want text/html: a method name is not a file name", p.ContentType)
	}
}

func TestDecodeYieldsTheRecordedWork(t *testing.T) {
	f, answer := loadFixture(t)
	p, err := web3.NewPlan(web3.Request{Contract: f.Contract, ChainID: f.ChainID, Path: "artpiece"}, web3.ModeAuto)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	body, err := p.Decode(answer)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(body) != f.BodyBytes {
		t.Fatalf("decoded %d bytes, want %d", len(body), f.BodyBytes)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != f.BodySha256 {
		t.Fatalf("decoded sha256 %s, want %s", got, f.BodySha256)
	}
	if !strings.HasPrefix(string(body), f.BodyPrefix) {
		t.Fatalf("decoded body does not start with the recorded prefix")
	}
	if !strings.HasSuffix(string(body), f.BodySuffix) {
		t.Fatalf("decoded body does not end with the recorded suffix")
	}
	// The work must be a whole document, not a truncated one: that is what
	// makes it playable with no sub-resource to fetch.
	if !strings.HasPrefix(string(body), "<!DOCTYPE html>") || !strings.HasSuffix(string(body), "</html>") {
		t.Fatalf("decoded body is not a whole HTML document")
	}
}

// TestAutoModeArgumentTyping pins ERC-4804's auto-mode rules. Art Blocks'
// generator is the case that matters: two arguments of different types, so a
// player resolves it with no function signature of its own.
func TestAutoModeArgumentTyping(t *testing.T) {
	const (
		generator = "0x953d288708bb771f969fcfd9ba0819ef506ac718"
		core      = "0x059edd72cd353df5106d2b9cc5ab83a52287ac3a"
	)
	cases := []struct {
		name     string
		path     string
		selector string // the signature the arguments' types imply
		tailHex  string
	}{
		{
			"address and uint256",
			"getTokenHtml/" + core + "/0",
			"getTokenHtml(address,uint256)",
			"000000000000000000000000" + core[2:] + strings.Repeat("0", 64),
		},
		{
			"a decimal is uint256",
			"tokenHTML/78000000",
			"tokenHTML(uint256)",
			"0000000000000000000000000000000000000000000000000000000004a62f80",
		},
		{
			"a 32-byte hex is bytes32, left aligned",
			"read/0x" + strings.Repeat("ab", 32),
			"read(bytes32)",
			strings.Repeat("ab", 32),
		},
		{
			"anything else is a string",
			"render/hello",
			"render(string)",
			// one head word holding the offset, then length and bytes
			"0000000000000000000000000000000000000000000000000000000000000020" +
				"0000000000000000000000000000000000000000000000000000000000000005" +
				"68656c6c6f" + strings.Repeat("0", 54),
		},
		{
			"an explicit type wins over the spelling",
			"render/string!123",
			"render(string)",
			"0000000000000000000000000000000000000000000000000000000000000020" +
				"0000000000000000000000000000000000000000000000000000000000000003" +
				"313233" + strings.Repeat("0", 58),
		},
		{
			"a percent-encoded segment is decoded before typing",
			"render/a%2Fb",
			"render(string)",
			"0000000000000000000000000000000000000000000000000000000000000020" +
				"0000000000000000000000000000000000000000000000000000000000000003" +
				"612f62" + strings.Repeat("0", 58),
		},
		{
			"no method is empty calldata",
			"",
			"",
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := web3.NewPlan(web3.Request{Contract: generator, ChainID: 1, Path: tc.path}, web3.ModeAuto)
			if err != nil {
				t.Fatalf("NewPlan(%q): %v", tc.path, err)
			}
			got := hex.EncodeToString(p.Calldata)
			if tc.selector == "" {
				if got != "" {
					t.Fatalf("calldata = %s, want empty", got)
				}
				return
			}
			want := hex.EncodeToString(web3.SelectorForTest(tc.selector)) + tc.tailHex
			if got != want {
				t.Fatalf("calldata for %q:\n got %s\nwant %s", tc.path, got, want)
			}
		})
	}
}

func TestManualModeSendsThePathAndQuery(t *testing.T) {
	// ERC-6860 manual mode: the contract parses the path itself, so the
	// player must send it verbatim and must not build a selector.
	p, err := web3.NewPlan(web3.Request{
		Contract: "0x953d288708bb771f969fcfd9ba0819ef506ac718", ChainID: 1,
		Path: "a/b", Query: "x=1",
	}, web3.ModeManual)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if string(p.Calldata) != "/a/b?x=1" {
		t.Fatalf("manual calldata = %q, want %q", p.Calldata, "/a/b?x=1")
	}
}

func TestContentTypeComesFromTheURLNotTheBytes(t *testing.T) {
	// A contract call carries no header, and sniffing would let the bytes
	// decide how they are rendered. The URL decides, defaulting to the HTML
	// these works are.
	cases := map[string]string{
		"artpiece":                "text/html",
		"getTokenHtml/0x01/1":     "text/html",
		"render/frame.svg":        "image/svg+xml",
		"render/thing.PNG":        "image/png",
		"render/data.json":        "application/json",
		"render/no.suchextension": "text/html",
	}
	for path, want := range cases {
		p, err := web3.NewPlan(web3.Request{Contract: "0x953d288708bb771f969fcfd9ba0819ef506ac718", ChainID: 1, Path: path}, web3.ModeAuto)
		if err != nil {
			t.Fatalf("NewPlan(%q): %v", path, err)
		}
		if p.ContentType != want {
			t.Errorf("ContentType for %q = %q, want %q", path, p.ContentType, want)
		}
	}
	// A returns attribute makes the answer JSON whatever the path says: the
	// decoded values are what is served, not the raw result.
	p, err := web3.NewPlan(web3.Request{Contract: "0x953d288708bb771f969fcfd9ba0819ef506ac718", ChainID: 1, Path: "render/frame.svg", Query: "returns=(string)"}, web3.ModeAuto)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if p.ContentType != "application/json" {
		t.Fatalf("ContentType with returns= = %q, want application/json", p.ContentType)
	}
}

func TestReturnsAttribute(t *testing.T) {
	contract := "0x953d288708bb771f969fcfd9ba0819ef506ac718"
	// returns=(string): one head word of offset, then length and bytes.
	answer, err := hex.DecodeString(
		"0000000000000000000000000000000000000000000000000000000000000020" +
			"0000000000000000000000000000000000000000000000000000000000000002" +
			"6869" + strings.Repeat("0", 60))
	if err != nil {
		t.Fatal(err)
	}
	p, err := web3.NewPlan(web3.Request{Contract: contract, ChainID: 1, Path: "name", Query: "returns=(string)"}, web3.ModeAuto)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	body, err := p.Decode(answer)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(body) != `["hi"]` {
		t.Fatalf("Decode = %s, want [\"hi\"]", body)
	}

	// returns=() is the standard's "hand me the raw result".
	p, err = web3.NewPlan(web3.Request{Contract: contract, ChainID: 1, Path: "name", Query: "returns=()"}, web3.ModeAuto)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	body, err = p.Decode(answer)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !strings.HasPrefix(string(body), `["0x0000`) {
		t.Fatalf("Decode with returns=() = %s, want the raw result hex-encoded", body)
	}
}

func TestPlanRefusesWhatItCannotDo(t *testing.T) {
	contract := "0x953d288708bb771f969fcfd9ba0819ef506ac718"
	for name, req := range map[string]web3.Request{
		"a returns list that is not a tuple": {Contract: contract, ChainID: 1, Path: "x", Query: "returns=string"},
		"an empty type in returns":           {Contract: contract, ChainID: 1, Path: "x", Query: "returns=(string,)"},
		"an argument type nothing encodes":   {Contract: contract, ChainID: 1, Path: "x/uint256[]!1"},
		"a malformed integer":                {Contract: contract, ChainID: 1, Path: "x/uint256!nope"},
		"a short address":                    {Contract: contract, ChainID: 1, Path: "x/address!0x01"},
	} {
		if _, err := web3.NewPlan(req, web3.ModeAuto); !errors.Is(err, web3.ErrUnsupported) {
			t.Errorf("%s: NewPlan err = %v, want ErrUnsupported", name, err)
		}
	}
	if _, err := web3.NewPlan(web3.Request{Contract: contract, ChainID: 1, Path: "x"}, "something-else"); !errors.Is(err, web3.ErrUnsupported) {
		t.Errorf("an unknown resolve mode: err = %v, want ErrUnsupported", err)
	}
}

func TestAQueryThatDoesNotParseStillPlays(t *testing.T) {
	// A work reads its seed from the query, and the query is carried to the
	// kiosk byte for byte whatever it holds. A query url.ParseQuery refuses
	// must not make the work unplayable; it only cannot carry a returns
	// attribute.
	p, err := web3.NewPlan(web3.Request{
		Contract: "0x4f32740cf412d647a622ba27c36fa4b76f71601f", ChainID: 1,
		Path: "artpiece", Query: "fxhash=0x06ec&%zz",
	}, web3.ModeAuto)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if p.HasReturns {
		t.Fatal("a query that does not parse must not yield a returns attribute")
	}
	if p.ContentType != "text/html" {
		t.Fatalf("ContentType = %q, want text/html", p.ContentType)
	}
}
