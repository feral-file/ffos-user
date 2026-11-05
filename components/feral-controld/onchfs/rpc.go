package onchfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// RPCCaller is a Caller over plain Ethereum JSON-RPC (eth_call), trying each
// endpoint in order until one answers. A contract revert (JSON-RPC error code
// 3, or an "execution reverted" message) is ErrNotFound; anything else is the
// transport's fault and is returned as such. Results are verified by the
// Reader, so a public endpoint is acceptable here.
type RPCCaller struct {
	Endpoints []string
	Client    *http.Client
}

// NewRPCCaller returns a caller over endpoints with a 60 s per-request timeout.
func NewRPCCaller(endpoints ...string) *RPCCaller {
	return &RPCCaller{Endpoints: endpoints, Client: &http.Client{Timeout: 60 * time.Second}}
}

// BlockNumber returns the endpoint's current head, for pinning a read.
func (c *RPCCaller) BlockNumber(ctx context.Context) (*big.Int, error) {
	raw, err := c.rpc(ctx, "eth_blockNumber", []any{})
	if err != nil {
		return nil, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("onchfs: eth_blockNumber: %w", err)
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("onchfs: eth_blockNumber returned %q", s)
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
		return nil, fmt.Errorf("onchfs: eth_call result: %w", err)
	}
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 != 0 || !isHexString(s) {
		return nil, fmt.Errorf("onchfs: eth_call returned non-hex result")
	}
	return mustHex(strings.ToLower(s)), nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *RPCCaller) rpc(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	var last error
	for _, ep := range c.Endpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Client.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			last = fmt.Errorf("%s: HTTP %d", ep, resp.StatusCode)
			continue
		}
		var out struct {
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			last = fmt.Errorf("%s: %w", ep, err)
			continue
		}
		if out.Error != nil {
			if out.Error.Code == 3 || strings.Contains(strings.ToLower(out.Error.Message), "revert") {
				return nil, ErrNotFound
			}
			last = fmt.Errorf("%s: rpc error %d: %s", ep, out.Error.Code, out.Error.Message)
			continue
		}
		if len(out.Result) == 0 {
			last = fmt.Errorf("%s: empty result", ep)
			continue
		}
		return out.Result, nil
	}
	if last == nil {
		last = errors.New("no endpoints")
	}
	return nil, fmt.Errorf("onchfs: every RPC endpoint failed: %w", last)
}

func isHexString(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
