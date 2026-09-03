package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/0xProject/rpc-gateway/internal/config"
)

const (
	healthCheckUserAgent = "rpc-gateway-health-check"
	maxRPCResponseBytes  = 1 << 20 // 1 MiB is plenty for a block number
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// callJSONRPC performs one JSON-RPC 2.0 call over HTTP and returns the raw
// result. Non-200 responses, malformed bodies and JSON-RPC errors are errors.
func callJSONRPC(ctx context.Context, client *http.Client, url, method string, params []any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", healthCheckUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var parsed rpcResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("invalid json-rpc response: %w (%s)", err, truncate(string(body), 200))
	}
	if parsed.Error != nil {
		return nil, parsed.Error
	}
	if len(parsed.Result) == 0 || string(parsed.Result) == "null" {
		return nil, fmt.Errorf("empty json-rpc result")
	}
	return parsed.Result, nil
}

// fetchHead returns the latest block number (EVM) or slot (Solana) of a node.
func fetchHead(ctx context.Context, client *http.Client, url string, typ config.ChainType) (uint64, error) {
	switch typ {
	case config.ChainTypeSolana:
		raw, err := callJSONRPC(ctx, client, url, "getSlot", nil)
		if err != nil {
			return 0, err
		}
		var slot uint64
		if err := json.Unmarshal(raw, &slot); err != nil {
			return 0, fmt.Errorf("getSlot: unexpected result %s", truncate(string(raw), 100))
		}
		return slot, nil
	default:
		raw, err := callJSONRPC(ctx, client, url, "eth_blockNumber", nil)
		if err != nil {
			return 0, err
		}
		var hex string
		if err := json.Unmarshal(raw, &hex); err != nil {
			return 0, fmt.Errorf("eth_blockNumber: unexpected result %s", truncate(string(raw), 100))
		}
		return parseHexUint64(hex)
	}
}

// parseHexUint64 parses a JSON-RPC quantity, which must be 0x-prefixed hex.
func parseHexUint64(s string) (uint64, error) {
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return 0, fmt.Errorf("invalid hex number %q: missing 0x prefix", s)
	}
	trimmed := s[2:]
	if trimmed == "" {
		return 0, fmt.Errorf("invalid hex number %q", s)
	}
	v, err := strconv.ParseUint(trimmed, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid hex number %q", s)
	}
	return v, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
