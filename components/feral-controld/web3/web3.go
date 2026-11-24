// Package web3 reads a work that is the return value of a contract call,
// addressed by an ERC-4804 `web3://` URL (with ERC-6860's corrections).
//
// A generative work does not have to live in a file. Han Saglam's Digital
// Soul is the return of `artpiece()` on
// 0x4f32740cf412d647a622ba27c36fa4b76f71601f, and Art Blocks' generator is
// `getTokenHtml(address,uint256)` on
// 0x953d288708bb771f969fcfd9ba0819ef506ac718. ERC-4804 gives both an address
// a player can resolve with one eth_call, with the method and its arguments
// in the URL, so nothing particular to one artist ever enters the player
// (feral-file#3599).
//
// # What is and is not guaranteed
//
// ipfs, ar and onchfs addresses commit to their bytes, so a reader can refuse
// a node that lies. A contract call does not: the guarantee is the chain's
// consensus, and so the node's. The device therefore does two things it
// cannot do for a content address, and no more:
//
//   - every call of one read is pinned to one block, so two nodes are asked
//     the same question;
//   - a second node's answer is compared with the first where a second node
//     answers, and a disagreement fails the read rather than picking one.
//
// A work still plays on one reachable node (see Resolver), so a single
// trusted-but-wrong node is the residual risk and it is stated rather than
// hidden.
//
// # Auto mode and manual mode
//
// `resolveMode()` is honored. A contract that answers "manual" is sent the
// path and query as raw calldata; one that answers "auto", answers nothing,
// or reverts (neither measured work implements resolveMode) is read in auto
// mode, where the first path segment is the method and the rest are its
// arguments, typed by the standard's rules:
//
//	type!value     the type, given explicitly
//	123            uint256
//	0x<64 hex>     bytes32
//	0x<40 hex>     address
//	0x<even hex>   bytes
//	anything else  string
//
// Without a `returns=` attribute the result is decoded as ABI bytes, which is
// also how a string comes back; with `returns=(…)` the decoded values are
// written as JSON, as the standard says.
//
// This is a Go port of display-protocol/dp1-capsule tools/web3-read.py, the
// reference the works above were reproduced with.
package web3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"path"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Errors a read ends with. ErrNotFound and ErrTooLarge are answers about the
// work; anything else is a failure of the node and another node may do
// better.
var (
	// ErrNotFound means the call reverted, or returned nothing: there is no
	// work at that address.
	ErrNotFound = errors.New("web3: no work at that address")
	// ErrTooLarge means the work exceeds MaxWorkBytes.
	ErrTooLarge = errors.New("web3: work is too large")
	// ErrDisagree means two nodes returned different bytes for the same call
	// at the same block. With no content address there is nothing to decide
	// between them, so the read fails.
	ErrDisagree = errors.New("web3: nodes disagree on the call's result")
	// ErrUnsupported means the URL asks for something this reader does not
	// do (an unknown resolve mode, an argument type outside auto mode's
	// rules).
	ErrUnsupported = errors.New("web3: unsupported web3:// URL")
)

// MaxWorkBytes bounds one work. The largest measured is Art Blocks' generator
// at 224 KiB; 16 MiB is ample for an HTML work and bounds the hex-encoded RPC
// answer at about 33 MiB.
const MaxWorkBytes = 16 << 20

// maxReturnsTypes bounds a `returns=` attribute, so a hostile playlist cannot
// make the decoder walk an unbounded head.
const maxReturnsTypes = 32

// Resolve modes ERC-6860 defines.
const (
	ModeAuto   = "auto"
	ModeManual = "manual"
)

// Request is one web3:// reference, as contentpath.Web3 parsed it. The
// package takes the parts rather than the URL so it never has to agree with
// contentpath about spelling.
type Request struct {
	// Contract is the 0x-prefixed contract address.
	Contract string
	// ChainID is the EIP-155 chain the call is made on.
	ChainID uint64
	// Path is the escaped URL path without a leading slash: the method, then
	// its arguments. "" is a call with empty calldata.
	Path string
	// Query is the raw query, without "?". ERC-4804's `returns=` lives here.
	Query string
}

// Work is one resolved call.
type Work struct {
	// Contract, ChainID and Block say exactly what was asked and where, so a
	// later reader can repeat it.
	Contract string
	ChainID  uint64
	Block    string
	// Mode is the resolve mode the contract asked for.
	Mode string
	// Calldata is what was sent, for the log and for a second node.
	Calldata []byte
	// Body is the work's bytes.
	Body []byte
	// ContentType is what the device will serve Body as. A call carries no
	// content type, so this is derived from the URL's own extension and
	// defaults to text/html; it is the one declared type on the device that
	// is ours rather than an origin's.
	ContentType string
	// ReadFrom is the endpoint whose answer Body came from.
	ReadFrom string
	// Confirmed says a second node returned the same bytes for the same
	// call at the same block, and ConfirmedBy names it. False means no
	// second node could answer, not that one disagreed: a disagreement
	// fails the read with ErrDisagree.
	Confirmed   bool
	ConfirmedBy string

	// plan is kept so a confirming node is asked the identical question and
	// its answer is read back the identical way.
	plan Plan
}

// mimeByExt is the extension-to-type table for a URL that names one. A work
// read from a contract has no header to carry a type, and a sniff would let
// the bytes decide how they are rendered (an HTML work served as
// text/plain shows its own source), so the URL decides and text/html is the
// default these works are.
var mimeByExt = map[string]string{
	".html": "text/html",
	".htm":  "text/html",
	".svg":  "image/svg+xml",
	".json": "application/json",
	".txt":  "text/plain",
	".js":   "text/javascript",
	".css":  "text/css",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".mp4":  "video/mp4",
	".webm": "video/webm",
}

// defaultContentType is what a work with no extension in its URL is served
// as. These works are HTML documents the kiosk navigates to.
const defaultContentType = "text/html"

// Caller makes one eth_call against one node at one block. Nothing it
// returns is trusted beyond what the package comment claims.
type Caller interface {
	Call(ctx context.Context, contract string, data []byte, block *big.Int) ([]byte, error)
}

// Plan is what a Request resolves to once the resolve mode is known: the
// calldata to send and how to read the answer back. Building it needs no
// network of its own — but it is not the first thing the resolver does: the
// real path spends eth_chainId, eth_blockNumber and resolveMode() on one
// endpoint before NewPlan, so a malformed URL costs three RPCs to that
// endpoint and then ends the read for every node (read() stops on
// ErrUnsupported).
type Plan struct {
	Mode        string
	Calldata    []byte
	Returns     []string
	HasReturns  bool
	ContentType string
}

// NewPlan builds the call for r under mode. mode is what resolveMode()
// answered; use ModeAuto when the contract has no resolveMode.
func NewPlan(r Request, mode string) (Plan, error) {
	switch mode {
	case ModeManual:
		// ERC-6860 manual mode: the path and query go to the contract as
		// raw calldata and the contract decides everything. Nothing in
		// auto mode's vocabulary applies, so the query is not read for a
		// `returns=` attribute (it is the contract's own query, carried
		// unchanged), the answer is decoded as ABI bytes, and the type is
		// the extension of the complete resource path.
		raw := "/" + r.Path
		if r.Query != "" {
			raw += "?" + r.Query
		}
		return Plan{
			Mode:        mode,
			Calldata:    []byte(raw),
			ContentType: contentTypeForPath(r.Path),
		}, nil
	case ModeAuto:
		return newAutoPlan(r)
	}
	return Plan{}, fmt.Errorf("%w: resolve mode %q", ErrUnsupported, mode)
}

// newAutoPlan is ERC-4804's auto mode: the first path segment is the method
// and the rest are its arguments, typed by their spelling.
func newAutoPlan(r Request) (Plan, error) {
	returns, hasReturns, err := parseReturns(r.Query)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Mode: ModeAuto, Returns: returns, HasReturns: hasReturns}
	segs, err := pathSegments(r.Path)
	if err != nil {
		return Plan{}, err
	}
	if len(segs) > 0 {
		args := make([]argument, 0, len(segs)-1)
		for _, raw := range segs[1:] {
			a, err := typeArgument(raw)
			if err != nil {
				return Plan{}, err
			}
			args = append(args, a)
		}
		types := make([]string, len(args))
		for i, a := range args {
			types[i] = a.typ
		}
		encoded, err := abiEncode(args)
		if err != nil {
			return Plan{}, err
		}
		p.Calldata = append(selector(segs[0]+"("+strings.Join(types, ",")+")"), encoded...)
	}
	// With no segment there is no method: ERC-4804's empty calldata, which
	// reaches the contract's fallback function.
	p.ContentType = contentTypeForAuto(segs, hasReturns)
	return p, nil
}

// Decode turns a node's answer into the work's bytes under p.
func (p Plan) Decode(out []byte) ([]byte, error) {
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: the call returned nothing", ErrNotFound)
	}
	if !p.HasReturns {
		// No returns attribute: the result is ABI-encoded bytes, which is
		// also how a string comes back. A result too short to carry an
		// offset and a length is taken as the raw bytes, as the reference
		// implementation does, so a contract returning a bare word still
		// shows something.
		if len(out) < 64 {
			return out, nil
		}
		body, err := abiDynamic(out, 0)
		if err != nil {
			return nil, err
		}
		return body, nil
	}
	values, err := abiDecodeValues(out, p.Returns)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// contentTypeForAuto reads the type off the URL's last segment. A `returns=`
// attribute makes the answer JSON whatever the path says, because the decoded
// values are what is served.
func contentTypeForAuto(segs []string, hasReturns bool) string {
	if hasReturns {
		return "application/json"
	}
	if len(segs) < 2 {
		// With no argument there is only the method name, and a method name
		// is not a file name: `/artpiece` must not be read as a work with no
		// extension *and* must not have its name parsed for one.
		return defaultContentType
	}
	return typeByExt(segs[len(segs)-1])
}

// contentTypeForPath is manual mode's: the whole path is the resource, so
// even a sole segment (`/image.svg`) names a file.
func contentTypeForPath(p string) string {
	last := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		last = p[i+1:]
	}
	if dec, err := url.PathUnescape(last); err == nil {
		last = dec
	}
	return typeByExt(last)
}

func typeByExt(name string) string {
	if ct, ok := mimeByExt[strings.ToLower(path.Ext(name))]; ok {
		return ct
	}
	return defaultContentType
}

// pathSegments splits and unescapes a URL path into its segments, dropping
// empty ones as ERC-4804 does.
func pathSegments(p string) ([]string, error) {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s == "" {
			continue
		}
		dec, err := url.PathUnescape(s)
		if err != nil {
			return nil, fmt.Errorf("%w: path segment %q is not valid percent-encoding", ErrUnsupported, s)
		}
		out = append(out, dec)
	}
	return out, nil
}

// parseReturns reads ERC-4804's `returns` (or `returnTypes`) attribute.
func parseReturns(rawQuery string) ([]string, bool, error) {
	if rawQuery == "" {
		return nil, false, nil
	}
	if _, err := url.ParseQuery(rawQuery); err != nil {
		// A query that does not parse is not an error for the call: a work's
		// own seed may be spelled in ways url.ParseQuery refuses, and the
		// query is carried to the kiosk byte for byte either way. It just
		// cannot carry a returns attribute.
		return nil, false, nil
	}
	// ERC-6860 defines returnTypes as an alias of returns and the last
	// attribute applies, whichever name it used, so walk the raw query in
	// order instead of checking one name before the other (round 6 review,
	// F13). q parsed, so every pair here unescapes.
	var key, last string
	found := false
	for _, pair := range strings.Split(rawQuery, "&") {
		k, v, _ := strings.Cut(pair, "=")
		k, kerr := url.QueryUnescape(k)
		v, verr := url.QueryUnescape(v)
		if kerr != nil || verr != nil || (k != "returns" && k != "returnTypes") {
			continue
		}
		key, last, found = k, v, true
	}
	if found {
		inner := strings.TrimSpace(last)
		if inner == "" {
			// ERC-6860 auto mode: an empty attribute is the default
			// decoding, the same as no attribute, and is distinct from
			// `returns=()` (the raw result). It must be answered before the
			// parentheses check, which would refuse it (round 5 review,
			// F10). As the last attribute it also cancels an earlier one.
			return nil, false, nil
		}
		if !strings.HasPrefix(inner, "(") || !strings.HasSuffix(inner, ")") {
			return nil, false, fmt.Errorf("%w: %s must look like (type,type)", ErrUnsupported, key)
		}
		inner = inner[1 : len(inner)-1]
		if strings.TrimSpace(inner) == "" {
			// `returns=()` is the standard's "give me the raw result": an
			// empty type list, not an absent attribute.
			return []string{}, true, nil
		}
		var types []string
		for _, t := range strings.Split(inner, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				return nil, false, fmt.Errorf("%w: %s has an empty type", ErrUnsupported, key)
			}
			types = append(types, t)
		}
		if len(types) > maxReturnsTypes {
			return nil, false, fmt.Errorf("%w: %s names %d types, more than %d", ErrUnsupported, key, len(types), maxReturnsTypes)
		}
		return types, true, nil
	}
	return nil, false, nil
}

// selector is the first four bytes of the keccak of a function signature.
func selector(signature string) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(signature))
	return h.Sum(nil)[:4]
}

func isHexDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func isDecimalDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
