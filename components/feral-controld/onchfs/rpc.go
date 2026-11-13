package onchfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The wire to public nodes. Each caller talks to exactly one endpoint: a tree
// is read from one node at one pinned block, and the Resolver moves to the
// next node when one fails. Mixing nodes inside a read would mix their views
// of the chain head.
//
// Nothing here is trusted. Answers are verified by the readers, a node on the
// wrong chain is caught by the Resolver's chain-id check, and bodies are
// capped so a hostile node cannot exhaust memory.

// DefaultRequestTimeout bounds one RPC request. The largest answer a work
// needs is one file's readFile (a 1 MB library is about 2 MB of hex), which
// public nodes return in well under a second.
const DefaultRequestTimeout = 15 * time.Second

// maxRPCBody caps one answer: a file at MaxPlainFileBytes, hex-encoded, plus
// the JSON around it.
const maxRPCBody = 2*MaxPlainFileBytes + 1<<20

// NewHTTPClient returns the client the callers use: timeout per request and
// no redirects. A redirect would take a request to a host nobody configured.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// RPCCaller is a Caller over Ethereum JSON-RPC (eth_call) to one endpoint. A
// contract revert (JSON-RPC error code 3, or an "execution reverted" message)
// is ErrNotFound; anything else is the endpoint's failure.
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
		return 0, fmt.Errorf("onchfs: %s: chain id %s out of range", c.Endpoint, n)
	}
	return n.Uint64(), nil
}

// BlockNumber returns the endpoint's current head, for pinning a read.
func (c *RPCCaller) BlockNumber(ctx context.Context) (*big.Int, error) {
	return c.quantity(ctx, "eth_blockNumber")
}

func (c *RPCCaller) quantity(ctx context.Context, method string) (*big.Int, error) {
	raw, err := c.rpc(ctx, method, []any{})
	if err != nil {
		return nil, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("onchfs: %s: %s: %w", c.Endpoint, method, err)
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("onchfs: %s: %s returned %q", c.Endpoint, method, s)
	}
	return n, nil
}

// Call implements Caller.
func (c *RPCCaller) Call(ctx context.Context, contract string, data []byte, block *big.Int) ([]byte, error) {
	tag := "latest"
	if block != nil {
		tag = "0x" + block.Text(16)
	}
	raw, err := c.rpc(ctx, "eth_call", []any{map[string]string{"to": contract, "data": "0x" + hexString(data)}, tag})
	if err != nil {
		return nil, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("onchfs: %s: eth_call result: %w", c.Endpoint, err)
	}
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 || !isHexString(s) {
		return nil, fmt.Errorf("onchfs: %s: eth_call returned a non-hex result", c.Endpoint)
	}
	return mustHex(strings.ToLower(s)), nil
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
		return nil, fmt.Errorf("onchfs: %s: HTTP %d", c.Endpoint, status)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("onchfs: %s: %w", c.Endpoint, err)
	}
	if out.Error != nil {
		if out.Error.Code == 3 || strings.Contains(strings.ToLower(out.Error.Message), "revert") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("onchfs: %s: rpc error %d: %s", c.Endpoint, out.Error.Code, out.Error.Message)
	}
	if len(out.Result) == 0 {
		return nil, fmt.Errorf("onchfs: %s: empty result", c.Endpoint)
	}
	return out.Result, nil
}

// TezosRPC is a TezosViewer over one Tezos node's RPC.
type TezosRPC struct {
	Endpoint string
	Client   *http.Client
}

// Head returns the node's chain id and head level.
func (c *TezosRPC) Head(ctx context.Context) (chainID string, level int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.Endpoint, "/")+"/chains/main/blocks/head/header", nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("onchfs: %s: %w", c.Endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("onchfs: %s: head: HTTP %d", c.Endpoint, resp.StatusCode)
	}
	var h struct {
		ChainID string `json:"chain_id"`
		Level   int64  `json:"level"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h); err != nil {
		return "", 0, fmt.Errorf("onchfs: %s: head: %w", c.Endpoint, err)
	}
	return h.ChainID, h.Level, nil
}

// View implements TezosViewer.
func (c *TezosRPC) View(ctx context.Context, block, view string, input json.RawMessage) (json.RawMessage, error) {
	if _, err := strconv.ParseInt(block, 10, 64); err != nil && !strings.HasPrefix(block, "B") {
		return nil, fmt.Errorf("onchfs: block %q is neither a level nor a hash", block)
	}
	body, err := json.Marshal(map[string]any{
		"contract":       TezosContract,
		"view":           view,
		"input":          input,
		"chain_id":       strings.TrimPrefix(TezosMainnet, "tezos:"),
		"unparsing_mode": "Optimized",
	})
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(c.Endpoint, "/") + "/chains/main/blocks/" + block + "/helpers/scripts/run_script_view"
	status, b, err := post(ctx, c.Client, url, body)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		// The contract fails a view on an unknown address with this string;
		// the node reports it as a script_rejected error with a 500.
		if bytes.Contains(b, []byte("script_rejected")) && bytes.Contains(b, []byte("ONCHFS_INODE_NOT_FOUND")) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("onchfs: %s: %s: HTTP %d", c.Endpoint, view, status)
	}
	var out struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Data) == 0 {
		return nil, fmt.Errorf("onchfs: %s: %s: malformed answer", c.Endpoint, view)
	}
	return out.Data, nil
}

// post sends a JSON body and returns the status and a capped body.
func post(ctx context.Context, client *http.Client, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("onchfs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCBody+1))
	if err != nil {
		return 0, nil, fmt.Errorf("onchfs: %s: %w", url, err)
	}
	if len(b) > maxRPCBody {
		return 0, nil, fmt.Errorf("onchfs: %s: answer exceeds %d bytes", url, maxRPCBody)
	}
	return resp.StatusCode, b, nil
}

func isHexString(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
