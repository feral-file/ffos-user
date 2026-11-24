package web3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Endpoints maps an EIP-155 chain id to the public nodes read from it, in
// order of preference.
type Endpoints map[uint64][]string

// DefaultEndpoints are the nodes a device reads from when its config names
// none: the same Ethereum and Base nodes the onchfs reader uses, each of
// which was checked on 2026-11-13 by reading a whole work through it
// (onchfs.DefaultEndpoints, docs/onchfs.md §3).
//
// Only these two chains are listed. ERC-4804 URLs can name any chain, but a
// chain with no node the device has read a work through is a chain it cannot
// serve, and guessing a public RPC for one would ship an unmeasured host of
// exactly the kind feral-file#3599 is about. A URL naming another chain is
// refused with an error that says so.
//
// Privacy: a node learns which contracts the device calls, as a gateway would
// learn which files it fetched. Feral File's own node is first where one
// exists.
var DefaultEndpoints = Endpoints{
	1:    {"https://rpc.feralfile.com", "https://ethereum-rpc.publicnode.com", "https://eth.drpc.org"},
	8453: {"https://mainnet.base.org", "https://base-rpc.publicnode.com", "https://base.drpc.org"},
}

// pinDepth is how far below a node's head a read is pinned. Public endpoints
// balance across nodes that can lag one another by a block or two; a block
// the second node has not seen yet would fail the confirmation.
const pinDepth = 3

// endpointCooldown is how long an endpoint that failed is moved to the back
// of its chain's list.
const endpointCooldown = time.Minute

// errWrongChain marks a node that answers for a chain other than the one it
// was configured for.
var errWrongChain = errors.New("web3: endpoint is on another chain")

// Resolver reads a web3:// work from the chain its URL names.
//
// A contract call has no content address, so unlike the onchfs resolver this
// one cannot tell a lying node from an honest one by checking the bytes. What
// it does instead:
//
//   - the whole read is pinned to one block, taken pinDepth below the first
//     node's head, so every node is asked the same question;
//   - once a node has answered, the next node in the list is asked the same
//     call at the same block. If it answers and the bytes differ, the read
//     fails with ErrDisagree rather than picking one.
//
// A second node that cannot answer — unreachable, or behind that block — is
// not a failure: the work still plays on one node, and a read that needed two
// would make every work on the device hostage to the second host being up,
// which is the failure feral-file#3599 exists to end. So the residual risk is
// a single node lying while the others are unreachable, and that is stated in
// docs/web3.md rather than hidden.
type Resolver struct {
	chains map[uint64]*chainReader
	logger *zap.Logger
}

// NewResolver builds a resolver over endpoints. Chains with no endpoints are
// skipped.
func NewResolver(endpoints Endpoints, client *http.Client, logger *zap.Logger) *Resolver {
	if client == nil {
		client = NewHTTPClient(DefaultRequestTimeout)
	}
	r := &Resolver{chains: map[uint64]*chainReader{}, logger: logger}
	for chain, eps := range endpoints {
		if len(eps) == 0 {
			continue
		}
		r.chains[chain] = &chainReader{
			chain:     chain,
			endpoints: append([]string(nil), eps...),
			client:    client,
			logger:    logger,
			cooling:   map[string]time.Time{},
			now:       time.Now,
		}
	}
	return r
}

// Chains lists the chain ids this resolver reads, in ascending order.
func (r *Resolver) Chains() []uint64 {
	out := make([]uint64, 0, len(r.chains))
	for chain := range r.chains {
		out = append(out, chain)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Resolve reads the work req names.
//
// ErrNotFound means the contract answered that there is no work there (the
// call reverted or returned nothing); ErrUnsupported means the URL asks for
// something this reader does not do, and no node will change that. Any other
// error means the work could not be read now and may be readable later.
func (r *Resolver) Resolve(ctx context.Context, req Request) (*Work, error) {
	c, ok := r.chains[req.ChainID]
	if !ok {
		return nil, fmt.Errorf("%w: no configured node for chain eip155:%d", ErrUnsupported, req.ChainID)
	}
	return c.read(ctx, req)
}

// chainReader reads from one chain's nodes.
type chainReader struct {
	chain     uint64
	endpoints []string
	client    *http.Client
	logger    *zap.Logger
	now       func() time.Time

	mu      sync.Mutex
	cooling map[string]time.Time // endpoint -> until; guarded by mu
}

// read tries this chain's nodes in order until one answers, then asks the
// next one that can for the same call at the same block.
func (c *chainReader) read(ctx context.Context, req Request) (*Work, error) {
	var last error
	order := c.order()
	for i, ep := range order {
		work, err := c.readFrom(ctx, ep, req)
		if err == nil {
			if err := c.confirm(ctx, order[i+1:], work); err != nil {
				return nil, err
			}
			return work, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// An answer about the work, not about the node: another node would
		// say the same thing, so stop.
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrTooLarge) || errors.Is(err, ErrUnsupported) {
			return nil, err
		}
		c.logger.Warn("web3: endpoint failed; trying the next",
			zap.Uint64("chain", c.chain), zap.String("endpoint", ep),
			zap.String("contract", req.Contract), zap.Error(err))
		c.cool(ep)
		last = err
	}
	if last == nil {
		last = fmt.Errorf("web3: chain eip155:%d has no endpoint", c.chain)
	}
	return nil, fmt.Errorf("web3: every eip155:%d endpoint failed: %w", c.chain, last)
}

// confirm asks the next reachable node the same call at the same block and
// compares the bytes, marking work confirmed on a match. A node that cannot
// answer is skipped and the read stands on one node's answer; a node that
// answers *differently* returns ErrDisagree, because with no content address
// there is nothing to decide between the two and showing either would be
// showing bytes the device cannot stand behind.
//
// "Differently" includes a revert. A node that reverts the call is not a node
// that could not answer: it is saying there is no work at that address, at
// the same block the first node returned bytes for.
func (c *chainReader) confirm(ctx context.Context, rest []string, work *Work) error {
	block, ok := new(big.Int).SetString(work.Block, 10)
	if !ok {
		return fmt.Errorf("web3: block %q is not a number", work.Block)
	}
	for _, ep := range rest {
		caller := &RPCCaller{Endpoint: ep, Client: c.client}
		if err := c.checkChain(ctx, caller, ep); err != nil {
			continue
		}
		out, err := caller.Call(ctx, work.Contract, work.Calldata, block)
		if errors.Is(err, errReverted) {
			// Not silence: this node, reachable and on the right chain, has
			// affirmatively answered "there is no work at that address" at
			// the very block the first node returned bytes for. That is the
			// strongest disagreement there is, and treating it as "no
			// opinion" is what let one dishonest node serve fabricated
			// bytes while an honest one said the work did not exist
			// (round 3 review, F-A).
			c.logger.Error("web3: a node reverted the call another node answered; the work is not shown",
				zap.Uint64("chain", c.chain), zap.String("contract", work.Contract),
				zap.String("answered", work.ReadFrom), zap.String("reverted", ep),
				zap.String("block", work.Block))
			return fmt.Errorf("%w: %s returned %d bytes at block %s where %s reverted",
				ErrDisagree, work.ReadFrom, len(work.Body), work.Block, ep)
		}
		if err != nil {
			// Unreachable, or behind that block: no second opinion to be
			// had from this node. Every error but a revert stays here on
			// purpose (the boundary the review's recommendation drew): a
			// revert is the node answering, while a transport failure or an
			// over-cap answer is ambiguous between a node's own bug and an
			// attack, and failing the read on those would let one flaky
			// endpoint make a work unplayable.
			continue
		}
		body, err := work.plan.Decode(out)
		if err != nil {
			// Same boundary: an answer that will not decode is not a
			// second opinion about the work's bytes.
			continue
		}
		if bytes.Equal(body, work.Body) {
			work.Confirmed, work.ConfirmedBy = true, ep
			return nil
		}
		c.logger.Error("web3: nodes disagree on a contract call; the work is not shown",
			zap.Uint64("chain", c.chain), zap.String("contract", work.Contract),
			zap.String("first", work.ReadFrom), zap.String("second", ep),
			zap.String("block", work.Block))
		return fmt.Errorf("%w: %s and %s differ at block %s (sha256 %s vs %s)",
			ErrDisagree, work.ReadFrom, ep, work.Block,
			shortDigest(work.Body), shortDigest(body))
	}
	c.logger.Info("web3: no second node confirmed the call; serving one node's answer",
		zap.Uint64("chain", c.chain), zap.String("contract", work.Contract),
		zap.String("endpoint", work.ReadFrom), zap.String("block", work.Block))
	return nil
}

func shortDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// readFrom reads the work from one node: check the chain, pin the block, ask
// resolveMode(), then make the call.
func (c *chainReader) readFrom(ctx context.Context, endpoint string, req Request) (*Work, error) {
	caller := &RPCCaller{Endpoint: endpoint, Client: c.client}
	if err := c.checkChain(ctx, caller, endpoint); err != nil {
		return nil, err
	}
	head, err := caller.BlockNumber(ctx)
	if err != nil {
		return nil, err
	}
	block := new(big.Int).Sub(head, big.NewInt(pinDepth))
	if block.Sign() < 0 {
		block.SetInt64(0)
	}
	mode, err := resolveMode(ctx, caller, req.Contract, block)
	if err != nil {
		return nil, err
	}
	plan, err := NewPlan(req, mode)
	if err != nil {
		return nil, err
	}
	out, err := caller.Call(ctx, req.Contract, plan.Calldata, block)
	if err != nil {
		if errors.Is(err, errReverted) {
			return nil, fmt.Errorf("%w: %s reverted %s", ErrNotFound, req.Contract, plan.Mode)
		}
		return nil, err
	}
	body, err := plan.Decode(out)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxWorkBytes {
		return nil, fmt.Errorf("%w: %d bytes, over %d", ErrTooLarge, len(body), MaxWorkBytes)
	}
	return &Work{
		Contract:    req.Contract,
		ChainID:     req.ChainID,
		Block:       block.String(),
		Mode:        plan.Mode,
		Calldata:    plan.Calldata,
		Body:        body,
		ContentType: plan.ContentType,
		ReadFrom:    endpoint,
		plan:        plan,
	}, nil
}

func (c *chainReader) checkChain(ctx context.Context, caller *RPCCaller, endpoint string) error {
	got, err := caller.ChainID(ctx)
	if err != nil {
		return err
	}
	if got != c.chain {
		// rpc.feralfile.com answers Base calls from Ethereum, so this check
		// is necessary and not a formality (docs/onchfs.md §3).
		return fmt.Errorf("%w: %s answers for eip155:%d", errWrongChain, endpoint, got)
	}
	return nil
}

// resolveMode asks the contract which mode ERC-6860 should read it in. A
// contract without resolveMode() reverts, returns nothing, or returns an
// empty word; all three mean auto, which is what both measured works are.
func resolveMode(ctx context.Context, caller Caller, contract string, block *big.Int) (string, error) {
	out, err := caller.Call(ctx, contract, selector("resolveMode()"), block)
	if err != nil {
		if errors.Is(err, errReverted) {
			return ModeAuto, nil
		}
		return "", err
	}
	if len(out) < wordBytes {
		return ModeAuto, nil
	}
	// The answer is a bytes32 holding a short ASCII name, NUL-padded.
	switch name := string(bytes.TrimRight(out[:wordBytes], "\x00")); name {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeManual:
		return ModeManual, nil
	default:
		return "", fmt.Errorf("%w: resolve mode %q", ErrUnsupported, name)
	}
}

// order lists endpoints in preference order with cooling ones moved to the
// back, so a node that just failed is still tried when nothing else is left.
func (c *chainReader) order() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var ready, cooling []string
	for _, ep := range c.endpoints {
		if until, ok := c.cooling[ep]; ok && now.Before(until) {
			cooling = append(cooling, ep)
			continue
		}
		ready = append(ready, ep)
	}
	return append(ready, cooling...)
}

func (c *chainReader) cool(ep string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cooling[ep] = c.now().Add(endpointCooldown)
}

// MergeEndpoints returns DefaultEndpoints with overrides applied: a chain in
// overrides replaces that chain's nodes, any other chain keeps its defaults.
// A chain the defaults do not know is added, so an operator can serve a chain
// the device does not ship a node for.
//
// Operator input is salvaged, not rejected whole (the same rule as
// onchfs.MergeEndpoints): each node must be an https URL with a host and no
// userinfo, query or fragment, and anything else is dropped and logged while
// the rest of the list still applies. A chain whose list salvages to nothing
// keeps its defaults rather than going dark.
func MergeEndpoints(overrides map[uint64][]string, logger *zap.Logger) Endpoints {
	out := Endpoints{}
	for chain, eps := range DefaultEndpoints {
		out[chain] = append([]string(nil), eps...)
	}
	for chain, eps := range overrides {
		if chain == 0 {
			logger.Error("web3: config names chain 0; ignored")
			continue
		}
		var kept []string
		for _, ep := range eps {
			if reason := endpointProblem(ep); reason != "" {
				logger.Error("web3: config node dropped",
					zap.Uint64("chain", chain), zap.String("endpoint", ep), zap.String("reason", reason))
				continue
			}
			kept = append(kept, strings.TrimSuffix(ep, "/"))
		}
		if len(kept) == 0 {
			logger.Error("web3: no usable configured node for chain; keeping defaults", zap.Uint64("chain", chain))
			continue
		}
		out[chain] = kept
	}
	return out
}

func endpointProblem(ep string) string {
	u, err := url.Parse(ep)
	switch {
	case err != nil:
		return "not a URL"
	case u.Scheme != "https":
		return "not https"
	case u.Host == "" || u.Hostname() == "":
		return "no host"
	case u.User != nil:
		return "carries credentials in userinfo"
	case u.RawQuery != "" || u.Fragment != "":
		return "carries a query or fragment"
	}
	return ""
}

// ChainIDFromString parses a decimal chain id, for config keys.
func ChainIDFromString(s string) (uint64, bool) {
	id, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(s), "eip155:"), 10, 64)
	return id, err == nil && id != 0
}
