package onchfs

import (
	"context"
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

// Endpoints maps a CAIP-2 chain to the public nodes read from it, in order of
// preference.
type Endpoints map[string][]string

// DefaultEndpoints are the nodes a device reads from when its config names
// none. Each was checked on 2026-11-13 by reading a whole work through it
// (Casey Reas, There's No Distance #1 on Ethereum; two fxhash trees on Tezos)
// or, for Base, a verified not-found. Nodes that refused (an API key, a 401 on
// run_script_view, rate limits on the first work) are not listed.
// rpc.feralfile.com serves Ethereum only: asked for Base it answers from
// Ethereum, which the chain-id check catches.
//
// Privacy: a node learns which onchfs roots the device reads, as a gateway
// would. Feral File's own node is first where one exists.
var DefaultEndpoints = Endpoints{
	"eip155:1":    {"https://rpc.feralfile.com", "https://ethereum-rpc.publicnode.com", "https://eth.drpc.org"},
	"eip155:8453": {"https://mainnet.base.org", "https://base-rpc.publicnode.com", "https://base.drpc.org"},
	TezosMainnet:  {"https://mainnet.smartpy.io", "https://rpc.tzkt.io/mainnet"},
}

// pinDepth is how far below a node's head a read is pinned. Public endpoints
// balance across nodes that can lag one another by a block or two; a block
// the next request's node has not seen yet would fail the read.
const pinDepth = 3

// endpointCooldown is how long an endpoint that failed is moved to the back
// of its chain's list.
const endpointCooldown = time.Minute

// errWrongChain marks a node that answers for a chain other than the one it
// was configured for.
var errWrongChain = errors.New("onchfs: endpoint is on another chain")

// Resolver reads an onchfs tree from whichever chain holds it.
//
// An onchfs URI usually names no chain (the indexer stores the bare
// `onchfs://<root>` form, as fxhash wrote it), so every configured chain is
// asked at once and the first verified tree wins, as onchfs-js's own resolver
// does. A root lives on at most one chain in practice; asking all of them
// costs one not-found read per other chain.
//
// Within a chain, nodes are tried in order. A node is left for the next one
// when it fails, answers for another chain, or returns bytes that do not
// verify. ErrNotFound and ErrTooLarge end the chain's attempt at once: they
// are answers about the work, not failures of the node.
type Resolver struct {
	chains []*chainReader
	logger *zap.Logger
}

// NewResolver builds a resolver over endpoints. Chains it cannot read
// (unknown CAIP-2 ids, no endpoints) are skipped with a warning.
func NewResolver(endpoints Endpoints, client *http.Client, logger *zap.Logger) *Resolver {
	if client == nil {
		client = NewHTTPClient(DefaultRequestTimeout)
	}
	r := &Resolver{logger: logger}
	for chain, eps := range endpoints {
		if len(eps) == 0 {
			continue
		}
		c := &chainReader{chain: chain, endpoints: append([]string(nil), eps...), client: client, logger: logger, cooling: map[string]time.Time{}, now: time.Now}
		switch {
		case chain == TezosMainnet:
			c.readFrom = c.readTezos
		case Contracts[chain] != "" && strings.HasPrefix(chain, "eip155:"):
			c.readFrom = c.readEVM
		default:
			logger.Warn("onchfs: no reader for chain; skipped", zap.String("chain", chain))
			continue
		}
		r.chains = append(r.chains, c)
	}
	return r
}

// Chains lists the chains this resolver reads.
func (r *Resolver) Chains() []string {
	out := make([]string, 0, len(r.chains))
	for _, c := range r.chains {
		out = append(out, c.chain)
	}
	return out
}

// Resolve reads root's whole tree.
//
// chain says where to look: a CAIP-2 id (an explicit URI authority) asks
// that chain only; contentpath.OnchfsEVM (the item's provenance said EVM)
// asks every EVM chain; "" (nothing known) asks the EVM chains first and
// Tezos only once every EVM chain has confirmed the root is absent.
//
// That ordering is a security rule, not a latency choice (PR #461 review
// F1). A Tezos file address commits to the content, an EVM one to the chunk
// list, so every EVM file address is also a valid Tezos address for bytes
// equal to the file's packed chunk checksums. Racing all chains would let one
// forged Tezos node pass those bytes off as an Ethereum work. The reverse
// forgery needs a keccak preimage, so EVM may be asked first freely; Tezos
// is asked only after EVM absence is confirmed by two nodes per chain
// (one when a chain has a single node), so a forged Tezos answer needs a
// lying EVM node as well.
//
// ErrNotFound means every chain asked answered that it holds no such root;
// any other error means the tree could not be read now and may be readable
// later.
func (r *Resolver) Resolve(ctx context.Context, root, chain string) (*Tree, error) {
	var evm, tezos, named []*chainReader
	for _, c := range r.chains {
		switch {
		case c.chain == chain:
			named = append(named, c)
		case strings.HasPrefix(c.chain, "eip155:"):
			evm = append(evm, c)
		default:
			tezos = append(tezos, c)
		}
	}
	switch chain {
	case "":
		t, err := r.resolveAmong(ctx, evm, root, true)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return t, err
		}
		return r.resolveAmong(ctx, tezos, root, false)
	case "eip155":
		return r.resolveAmong(ctx, evm, root, false)
	}
	if len(named) == 0 {
		return nil, fmt.Errorf("onchfs: no configured chain to read %s from (asked for %q)", root, chain)
	}
	return r.resolveAmong(ctx, named, root, false)
}

// resolveAmong asks chains at once and keeps the first verified tree.
// confirmAbsence makes each chain confirm a not-found with a second node.
func (r *Resolver) resolveAmong(ctx context.Context, chains []*chainReader, root string, confirmAbsence bool) (*Tree, error) {
	if len(chains) == 0 {
		return nil, fmt.Errorf("%w: no configured chain to read %s from", ErrNotFound, root)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		tree *Tree
		err  error
	}
	results := make(chan result, len(chains))
	for _, c := range chains {
		go func() {
			t, err := c.read(ctx, root, confirmAbsence)
			results <- result{t, err}
		}()
	}
	var errs []error
	notFound := 0
	for range chains {
		res := <-results
		if res.err == nil {
			return res.tree, nil
		}
		if errors.Is(res.err, ErrNotFound) {
			notFound++
			continue
		}
		errs = append(errs, res.err)
	}
	if notFound == len(chains) {
		return nil, fmt.Errorf("%w: root %s is on none of %d chains", ErrNotFound, root, len(chains))
	}
	for _, err := range errs {
		if errors.Is(err, ErrTooLarge) {
			return nil, err
		}
	}
	return nil, errors.Join(errs...)
}

// chainReader reads from one chain's nodes.
type chainReader struct {
	chain     string
	endpoints []string
	client    *http.Client
	logger    *zap.Logger
	readFrom  func(ctx context.Context, endpoint, root string) (*Tree, error)
	now       func() time.Time

	mu      sync.Mutex
	cooling map[string]time.Time // endpoint -> until; guarded by mu
}

// read reads root from this chain's nodes, one at a time. With
// confirmAbsence, a not-found is believed only once a second node agrees
// (see Resolver.Resolve); a node that then serves the tree wins, and the
// node that denied it is logged.
func (c *chainReader) read(ctx context.Context, root string, confirmAbsence bool) (*Tree, error) {
	var last error
	order := c.order()
	denied := ""
	for _, ep := range order {
		t, err := c.readFrom(ctx, ep, root)
		if err == nil {
			if denied != "" {
				c.logger.Warn("onchfs: a node denied a root another node served",
					zap.String("chain", c.chain), zap.String("endpoint", denied), zap.String("root", root))
				c.cool(denied)
			}
			return t, nil
		}
		if ctx.Err() != nil || errors.Is(err, ErrTooLarge) {
			return nil, err
		}
		if errors.Is(err, ErrNotFound) {
			if !confirmAbsence || denied != "" || len(order) == 1 {
				return nil, err
			}
			denied = ep
			continue
		}
		c.logger.Warn("onchfs: endpoint failed; trying the next",
			zap.String("chain", c.chain), zap.String("endpoint", ep), zap.String("root", root), zap.Error(err))
		c.cool(ep)
		last = err
	}
	if denied != "" {
		return nil, fmt.Errorf("onchfs: %s absence of %s not confirmed by a second node: %w", c.chain, root, last)
	}
	return nil, fmt.Errorf("onchfs: every %s endpoint failed: %w", c.chain, last)
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

func (c *chainReader) readEVM(ctx context.Context, endpoint, root string) (*Tree, error) {
	caller := &RPCCaller{Endpoint: endpoint, Client: c.client}
	want, _ := strconv.ParseUint(strings.TrimPrefix(c.chain, "eip155:"), 10, 64)
	got, err := caller.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("%w: %s answers for eip155:%d", errWrongChain, endpoint, got)
	}
	head, err := caller.BlockNumber(ctx)
	if err != nil {
		return nil, err
	}
	block := new(big.Int).Sub(head, big.NewInt(pinDepth))
	if block.Sign() < 0 {
		block.SetInt64(0)
	}
	r, err := NewReader(caller, c.chain, block)
	if err != nil {
		return nil, err
	}
	return r.ReadTree(ctx, root)
}

func (c *chainReader) readTezos(ctx context.Context, endpoint, root string) (*Tree, error) {
	node := &TezosRPC{Endpoint: endpoint, Client: c.client}
	chainID, level, err := node.Head(ctx)
	if err != nil {
		return nil, err
	}
	if "tezos:"+chainID != TezosMainnet {
		return nil, fmt.Errorf("%w: %s answers for tezos:%s", errWrongChain, endpoint, chainID)
	}
	return NewTezosReader(node, strconv.FormatInt(max(level-pinDepth, 0), 10)).ReadTree(ctx, root)
}

// MergeEndpoints returns DefaultEndpoints with overrides applied: a chain in
// overrides replaces that chain's nodes, any other chain keeps its defaults.
//
// Operator input is salvaged, not rejected whole (the gatewayUserAgent rule):
// each node must be an https URL with a host and no userinfo, query or
// fragment, and anything else is dropped and logged while the rest of the
// list still applies. A chain whose list salvages to nothing keeps its
// defaults rather than going dark, and a chain the device has no reader for
// is dropped.
func MergeEndpoints(overrides map[string][]string, logger *zap.Logger) Endpoints {
	out := Endpoints{}
	for chain, eps := range DefaultEndpoints {
		out[chain] = append([]string(nil), eps...)
	}
	for chain, eps := range overrides {
		if _, known := DefaultEndpoints[chain]; !known {
			logger.Error("onchfs: config names a chain the device cannot read; ignored", zap.String("chain", chain))
			continue
		}
		var kept []string
		for _, ep := range eps {
			if reason := endpointProblem(ep); reason != "" {
				logger.Error("onchfs: config node dropped", zap.String("chain", chain), zap.String("endpoint", ep), zap.String("reason", reason))
				continue
			}
			kept = append(kept, strings.TrimSuffix(ep, "/"))
		}
		if len(kept) == 0 {
			logger.Error("onchfs: no usable configured node for chain; keeping defaults", zap.String("chain", chain))
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
