package web3_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/web3"
)

const (
	testContract = "0x4f32740cf412d647a622ba27c36fa4b76f71601f"
	// artpieceSelector is keccak("artpiece()")[:4].
	artpieceSelector = "0x37a908fc"
	// resolveModeSelector is keccak("resolveMode()")[:4].
	resolveModeSelector = "0xdd473fae"
)

// node is a fake JSON-RPC endpoint. Each field says how it answers one
// method, so a test can build the exact node it needs: one on the wrong
// chain, one that reverts, one that lies about the bytes.
type node struct {
	chainID uint64
	head    uint64
	// answers maps a calldata prefix (the selector) to the hex result the
	// node returns. A selector with no entry reverts.
	answers map[string]string
	// httpStatus, when non-zero, is returned instead of answering.
	httpStatus int
	calls      atomic.Int64
	// lastBlock records the block tag of the last eth_call, so a test can
	// assert both nodes were asked the same question.
	lastBlock atomic.Value
}

func (n *node) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.httpStatus != 0 {
			w.WriteHeader(n.httpStatus)
			return
		}
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply := func(result string) {
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%q}`, result)
		}
		switch req.Method {
		case "eth_chainId":
			reply(fmt.Sprintf("0x%x", n.chainID))
		case "eth_blockNumber":
			reply(fmt.Sprintf("0x%x", n.head))
		case "eth_call":
			n.calls.Add(1)
			var call struct {
				Data string `json:"data"`
			}
			_ = json.Unmarshal(req.Params[0], &call)
			var tag string
			_ = json.Unmarshal(req.Params[1], &tag)
			n.lastBlock.Store(tag)
			for sel, res := range n.answers {
				if strings.HasPrefix(call.Data, sel) {
					reply(res)
					return
				}
			}
			_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"execution reverted"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// abiBytes is s ABI-encoded as the dynamic bytes a view function returns.
func abiBytes(s string) string {
	body := []byte(s)
	pad := (32 - len(body)%32) % 32
	return "0x" +
		"0000000000000000000000000000000000000000000000000000000000000020" +
		fmt.Sprintf("%064x", len(body)) +
		hex.EncodeToString(body) + strings.Repeat("00", pad)
}

func resolverOver(t *testing.T, chain uint64, urls ...string) *web3.Resolver {
	t.Helper()
	// An httptest server is http, not https, so the endpoints are given to
	// NewResolver directly rather than through MergeEndpoints (which
	// requires https, as an operator's config must).
	return web3.NewResolver(web3.Endpoints{chain: urls}, nil, zaptest.NewLogger(t))
}

func TestResolveReadsAWorkAndConfirmsItWithASecondNode(t *testing.T) {
	const want = "<!DOCTYPE html><html>a work</html>"
	first := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes(want)}}
	second := &node{chainID: 1, head: 104, answers: map[string]string{artpieceSelector: abiBytes(want)}}
	r := resolverOver(t, 1, first.serve(t), second.serve(t))

	work, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(work.Body) != want {
		t.Fatalf("Body = %q, want %q", work.Body, want)
	}
	if work.Mode != web3.ModeAuto {
		t.Fatalf("Mode = %q, want auto: a contract whose resolveMode() reverts is auto", work.Mode)
	}
	if !work.Confirmed {
		t.Fatal("Confirmed = false, want a second node's agreement")
	}
	if work.Block != "97" { // 100 - pinDepth
		t.Fatalf("Block = %q, want 97 (the first node's head less the pin depth)", work.Block)
	}
	// Both nodes must be asked the same block, or two honest nodes could
	// legitimately differ and the comparison would mean nothing.
	if got := second.lastBlock.Load(); got != "0x61" {
		t.Fatalf("the confirming node was asked block %v, want 0x61 (97)", got)
	}
	if work.ContentType != "text/html" {
		t.Fatalf("ContentType = %q, want text/html", work.ContentType)
	}
}

func TestResolveFailsWhenNodesDisagree(t *testing.T) {
	// With no content address there is nothing to decide between two
	// answers, so showing either would be showing bytes the device cannot
	// stand behind.
	first := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes("the work")}}
	liar := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes("something else")}}
	r := resolverOver(t, 1, first.serve(t), liar.serve(t))

	_, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if !errors.Is(err, web3.ErrDisagree) {
		t.Fatalf("Resolve err = %v, want ErrDisagree", err)
	}
}

func TestResolveServesOneNodeWhenNoSecondAnswers(t *testing.T) {
	// A work must not be hostage to a second host being up: that is the
	// failure feral-file#3599 exists to end. One reachable node plays the
	// work, and the record says it was not confirmed.
	const want = "the work"
	first := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes(want)}}
	dead := &node{chainID: 1, httpStatus: http.StatusBadGateway}
	r := resolverOver(t, 1, first.serve(t), dead.serve(t))

	work, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(work.Body) != want {
		t.Fatalf("Body = %q, want %q", work.Body, want)
	}
	if work.Confirmed {
		t.Fatal("Confirmed = true, but no second node answered")
	}
}

func TestResolveMovesPastAFailingNode(t *testing.T) {
	const want = "the work"
	dead := &node{chainID: 1, httpStatus: http.StatusInternalServerError}
	good := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes(want)}}
	r := resolverOver(t, 1, dead.serve(t), good.serve(t))

	work, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(work.Body) != want {
		t.Fatalf("Body = %q, want %q", work.Body, want)
	}
}

func TestResolveRefusesANodeOnAnotherChain(t *testing.T) {
	// rpc.feralfile.com answers Base calls from Ethereum, so this check is
	// load-bearing and not a formality (docs/onchfs.md §3).
	wrong := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes("ethereum's answer")}}
	r := resolverOver(t, 8453, wrong.serve(t))

	if _, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 8453, Path: "artpiece"}); err == nil {
		t.Fatal("Resolve accepted a node answering for another chain")
	}
	if wrong.calls.Load() != 0 {
		t.Fatalf("the wrong-chain node was called %d times, want 0", wrong.calls.Load())
	}
}

func TestResolveReportsNotFoundWhenTheCallReverts(t *testing.T) {
	// A revert is an answer about the work, so no other node is asked.
	empty := &node{chainID: 1, head: 100}
	second := &node{chainID: 1, head: 100}
	r := resolverOver(t, 1, empty.serve(t), second.serve(t))

	_, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if !errors.Is(err, web3.ErrNotFound) {
		t.Fatalf("Resolve err = %v, want ErrNotFound", err)
	}
	if second.calls.Load() != 0 {
		t.Fatalf("the second node was asked %d times after a revert, want 0", second.calls.Load())
	}
}

func TestResolveHonoursManualMode(t *testing.T) {
	// A contract that answers "manual" is sent the path and query as raw
	// calldata; nothing artist-specific enters the player either way.
	manual := "0x" + hex.EncodeToString(append([]byte("manual"), make([]byte, 26)...))
	n := &node{chainID: 1, head: 100, answers: map[string]string{
		resolveModeSelector: manual,
		// The manual calldata is "/a/b?x=1", whose hex begins 2f61.
		"0x2f612f62": abiBytes("manual work"),
	}}
	r := resolverOver(t, 1, n.serve(t))

	work, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "a/b", Query: "x=1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if work.Mode != web3.ModeManual {
		t.Fatalf("Mode = %q, want manual", work.Mode)
	}
	if string(work.Body) != "manual work" {
		t.Fatalf("Body = %q, want %q", work.Body, "manual work")
	}
}

func TestResolveRefusesAnUnknownResolveMode(t *testing.T) {
	odd := "0x" + hex.EncodeToString(append([]byte("something"), make([]byte, 23)...))
	n := &node{chainID: 1, head: 100, answers: map[string]string{resolveModeSelector: odd}}
	r := resolverOver(t, 1, n.serve(t))

	_, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if !errors.Is(err, web3.ErrUnsupported) {
		t.Fatalf("Resolve err = %v, want ErrUnsupported", err)
	}
}

func TestResolveRefusesAChainWithNoNode(t *testing.T) {
	// A chain with no node the device has read a work through is a chain it
	// cannot serve. Guessing a public RPC for one would ship an unmeasured
	// host of exactly the kind this work is about.
	r := resolverOver(t, 1, (&node{chainID: 1, head: 100}).serve(t))
	_, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 999999, Path: "artpiece"})
	if !errors.Is(err, web3.ErrUnsupported) {
		t.Fatalf("Resolve err = %v, want ErrUnsupported", err)
	}
}

func TestDefaultEndpointsAreTheMeasuredChains(t *testing.T) {
	r := web3.NewResolver(web3.DefaultEndpoints, nil, zaptest.NewLogger(t))
	got := r.Chains()
	if len(got) != 2 || got[0] != 1 || got[1] != 8453 {
		t.Fatalf("Chains() = %v, want [1 8453]", got)
	}
	for chain, eps := range web3.DefaultEndpoints {
		for _, ep := range eps {
			if !strings.HasPrefix(ep, "https://") {
				t.Errorf("chain %d default node %q is not https", chain, ep)
			}
		}
	}
}

func TestMergeEndpointsSalvagesOperatorInput(t *testing.T) {
	logger := zaptest.NewLogger(t)
	out := web3.MergeEndpoints(map[uint64][]string{
		1: {"http://insecure.example", "https://good.example/", "https://creds@bad.example", "https://q.example?k=v", "not a url at all"},
	}, logger)
	if got := out[1]; len(got) != 1 || got[0] != "https://good.example" {
		t.Fatalf("chain 1 = %v, want the one salvaged https node with its trailing slash trimmed", got)
	}
	// A chain not overridden keeps its defaults.
	if len(out[8453]) != len(web3.DefaultEndpoints[8453]) {
		t.Fatalf("chain 8453 = %v, want the defaults", out[8453])
	}
	// A chain whose list salvages to nothing keeps its defaults rather than
	// going dark.
	out = web3.MergeEndpoints(map[uint64][]string{1: {"http://nope.example"}}, logger)
	if len(out[1]) != len(web3.DefaultEndpoints[1]) {
		t.Fatalf("chain 1 = %v, want the defaults kept", out[1])
	}
	// A chain the defaults do not know is added, so an operator can serve a
	// chain the device ships no node for.
	out = web3.MergeEndpoints(map[uint64][]string{10: {"https://optimism.example"}}, logger)
	if got := out[10]; len(got) != 1 || got[0] != "https://optimism.example" {
		t.Fatalf("chain 10 = %v, want the configured node", got)
	}
	// Chain 0 is not a chain.
	if _, ok := web3.MergeEndpoints(map[uint64][]string{0: {"https://x.example"}}, logger)[0]; ok {
		t.Fatal("chain 0 was accepted")
	}
}

func TestChainIDFromString(t *testing.T) {
	for in, want := range map[string]uint64{"1": 1, "8453": 8453, "eip155:1": 1, " 10 ": 10} {
		got, ok := web3.ChainIDFromString(in)
		if !ok || got != want {
			t.Errorf("ChainIDFromString(%q) = %d/%v, want %d/true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0", "eip155:0", "mainnet", "tezos:NetXdQprcVkpaWU", "-1"} {
		if got, ok := web3.ChainIDFromString(in); ok {
			t.Errorf("ChainIDFromString(%q) = %d/true, want refused", in, got)
		}
	}
}

// A second node that REVERTS the call is not a node that could not answer: it
// is saying there is no work at that address, at the very block the first node
// returned bytes for. That is the strongest disagreement there is, and it used
// to be swallowed as "no opinion", so one dishonest node's fabricated bytes
// were served while an honest node said the work did not exist (round 3
// review, F-A). No earlier test covered it: the revert test reverts the FIRST
// node, and the no-second-opinion test uses an HTTP 502.
func TestResolveFailsWhenASecondNodeRevertsWhatTheFirstAnswered(t *testing.T) {
	liar := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes("fabricated")}}
	// No answers: every eth_call reverts, including the work call.
	honest := &node{chainID: 1, head: 100}
	r := resolverOver(t, 1, liar.serve(t), honest.serve(t))

	_, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if !errors.Is(err, web3.ErrDisagree) {
		t.Fatalf("Resolve err = %v, want ErrDisagree: the second node reverted what the first answered", err)
	}
	if honest.calls.Load() == 0 {
		t.Fatal("the second node was never asked, so the test proves nothing")
	}
}

// The boundary the fix draws, stated as a test: only a revert is an answer. A
// node that cannot be reached at all is still "no opinion", so one flaky
// endpoint cannot make a work unplayable.
func TestASecondNodeThatCannotBeReachedIsStillNoOpinion(t *testing.T) {
	const want = "the work"
	first := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes(want)}}
	unreachable := &node{chainID: 1, httpStatus: http.StatusGatewayTimeout}
	r := resolverOver(t, 1, first.serve(t), unreachable.serve(t))

	work, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(work.Body) != want {
		t.Fatalf("Body = %q, want %q", work.Body, want)
	}
	if work.Confirmed {
		t.Fatal("Confirmed = true, but no second node answered")
	}
}

// A second node that RETURNS NOTHING ("0x") is answering too: ErrNotFound's own
// doc is "the call reverted, or returned nothing". It was swallowed as an
// undecodable answer, so a fabricated body was served with Confirmed=false
// and no error (round 5 review, F7). The boundary stays: a non-empty answer
// that will not decode is still no opinion.
func TestResolveFailsWhenASecondNodeReturnsNothingWhatTheFirstAnswered(t *testing.T) {
	liar := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes("fabricated")}}
	empty := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: "0x"}}
	r := resolverOver(t, 1, liar.serve(t), empty.serve(t))

	_, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if !errors.Is(err, web3.ErrDisagree) {
		t.Fatalf("Resolve err = %v, want ErrDisagree: the second node returned nothing", err)
	}
}

func TestASecondNodeWithAnUndecodableAnswerIsStillNoOpinion(t *testing.T) {
	const want = "the work"
	first := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: abiBytes(want)}}
	// 64 bytes whose offset points far past the answer: non-empty, not decodable.
	junk := &node{chainID: 1, head: 100, answers: map[string]string{artpieceSelector: "0x" + strings.Repeat("ff", 64)}}
	r := resolverOver(t, 1, first.serve(t), junk.serve(t))

	work, err := r.Resolve(context.Background(), web3.Request{Contract: testContract, ChainID: 1, Path: "artpiece"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(work.Body) != want || work.Confirmed {
		t.Fatalf("Body = %q Confirmed = %v, want %q unconfirmed", work.Body, work.Confirmed, want)
	}
}
