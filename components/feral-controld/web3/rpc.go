package web3

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// The wire to public nodes. One caller talks to exactly one endpoint, as the
// onchfs callers do: a work is read from one node at one pinned block, and
// the Resolver moves to the next node when one fails. Mixing nodes inside one
// read would mix their views of the chain head.
//
// This is a sibling of onchfs/rpc.go rather than a shared package, because
// the two differ in what the wire has to mean. For onchfs a revert is always
// "no such inode"; here it is either "the contract has no resolveMode()" or
// "no work at that address", and only the caller knows which call it made.
// onchfs also caps an answer at twice its 64 MiB file limit, where a contract
// call is bounded by MaxWorkBytes. One type carrying both sets of rules would
// need a mode flag for each of them.

// DefaultRequestTimeout bounds one RPC request. The largest work measured
// (Art Blocks' generator, 224 KiB, about 448 KiB of hex) returns in well
// under a second from the default nodes.
const DefaultRequestTimeout = 15 * time.Second

// maxRPCBody caps one answer: a work at MaxWorkBytes, hex-encoded, plus the
// JSON around it.
const maxRPCBody = 2*MaxWorkBytes + 1<<20

// errReverted marks a call the contract refused. Which work-level error that
// means is the caller's to decide: a reverting resolveMode() is a contract
// without one, a reverting work call is ErrNotFound.
var errReverted = errors.New("web3: the call reverted")

// NewHTTPClient returns the client the callers use: a timeout per request and
// no redirects, because a redirect would take a request to a host nobody
// configured.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// RPCCaller is a Caller over Ethereum JSON-RPC to one endpoint.
type RPCCaller struct {
	Endpoint string
	Client   *http.Client
}

// NewRPCCaller returns a caller to endpoint with DefaultRequestTimeout.
func NewRPCCaller(endpoint string) *RPCCaller {
	return &RPCCaller{Endpoint: endpoint, Client: NewHTTPClient(DefaultRequestTimeout)}
}

// ChainID returns the endpoint's eth_chainId.
func (c *RPCCaller) ChainID(ctx context.Context) (uint64, error) {
	n, err := c.quantity(ctx, "eth_chainId")
	if err != nil {
		return 0, err
	}
	if !n.IsUint64() {
		return 0, fmt.Errorf("web3: %s: chain id %s out of range", c.Endpoint, n)
	}
	return n.Uint64(), nil
}

// BlockNumber returns the endpoint's current head, for pinning a read.
func (c *RPCCaller) BlockNumber(ctx context.Context) (*big.Int, error) {
	return c.quantity(ctx, "eth_blockNumber")
}

// Call implements Caller. A revert is errReverted; anything else is the
// endpoint's failure.
func (c *RPCCaller) Call(ctx context.Context, contract string, data []byte, block *big.Int) ([]byte, error) {
	tag := "latest"
	if block != nil {
		tag = "0x" + block.Text(16)
	}
	raw, err := c.rpc(ctx, "eth_call", []any{map[string]string{"to": contract, "data": "0x" + hex.EncodeToString(data)}, tag})
	if err != nil {
		return nil, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("web3: %s: eth_call result: %w", c.Endpoint, err)
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	out, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("web3: %s: eth_call returned a non-hex result", c.Endpoint)
	}
	if len(out) > MaxWorkBytes {
		return nil, fmt.Errorf("%w: %s returned %d bytes, over %d", ErrTooLarge, c.Endpoint, len(out), MaxWorkBytes)
	}
	return out, nil
}

func (c *RPCCaller) quantity(ctx context.Context, method string) (*big.Int, error) {
	raw, err := c.rpc(ctx, method, []any{})
	if err != nil {
		return nil, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("web3: %s: %s: %w", c.Endpoint, method, err)
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("web3: %s: %s returned %q", c.Endpoint, method, s)
	}
	return n, nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *RPCCaller) rpc(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	status, b, err := post(ctx, c.Client, c.Endpoint, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("web3: %s: HTTP %d", c.Endpoint, status)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("web3: %s: %w", c.Endpoint, err)
	}
	if out.Error != nil {
		// JSON-RPC code 3 is the execution-reverted code; some nodes send
		// another code with the message instead.
		if out.Error.Code == 3 || strings.Contains(strings.ToLower(out.Error.Message), "revert") {
			return nil, errReverted
		}
		return nil, fmt.Errorf("web3: %s: rpc error %d: %s", c.Endpoint, out.Error.Code, out.Error.Message)
	}
	if len(out.Result) == 0 {
		return nil, fmt.Errorf("web3: %s: empty result", c.Endpoint)
	}
	return out.Result, nil
}

// post sends a JSON body and returns the status and a capped body.
func post(ctx context.Context, client *http.Client, endpoint string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("web3: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCBody+1))
	if err != nil {
		return 0, nil, fmt.Errorf("web3: %s: %w", endpoint, err)
	}
	if len(b) > maxRPCBody {
		return 0, nil, fmt.Errorf("web3: %s: answer exceeds %d bytes", endpoint, maxRPCBody)
	}
	return resp.StatusCode, b, nil
}
