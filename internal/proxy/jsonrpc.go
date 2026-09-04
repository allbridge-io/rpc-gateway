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

// postJSON sends a JSON body with the target's extra headers and returns the
// response body. A non-200 status is an error.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", healthCheckUserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

// callJSONRPC performs one JSON-RPC 2.0 call over HTTP and returns the raw
// result. Non-200 responses, malformed bodies and JSON-RPC errors are errors.
func callJSONRPC(ctx context.Context, client *http.Client, url string, headers map[string]string, method string, params []any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	body, err := postJSON(ctx, client, url, headers, payload)
	if err != nil {
		return nil, err
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

// tronNowBlock is the part of /wallet/getnowblock we care about.
type tronNowBlock struct {
	BlockHeader struct {
		RawData struct {
			Number uint64 `json:"number"`
		} `json:"raw_data"`
	} `json:"block_header"`
	Error string `json:"Error"`
}

// JoinURLPath appends a sub-path to a base URL that may already have a path.
func JoinURLPath(base, sub string) string {
	if sub == "" || sub == "/" {
		return base
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(sub, "/")
}

// fetchHead returns the latest block number (EVM, Tron) or slot (Solana) of a target.
func fetchHead(ctx context.Context, client *http.Client, t config.Target, typ config.ChainType) (uint64, error) {
	url := t.HTTPURL
	switch typ {
	case config.ChainTypeTron:
		body, err := postJSON(ctx, client, JoinURLPath(url, "/wallet/getnowblock"), t.Headers, []byte("{}"))
		if err != nil {
			return 0, err
		}
		var parsed tronNowBlock
		if err := json.Unmarshal(body, &parsed); err != nil {
			return 0, fmt.Errorf("getnowblock: invalid response: %w (%s)", err, truncate(string(body), 200))
		}
		if parsed.Error != "" {
			return 0, fmt.Errorf("getnowblock: %s", parsed.Error)
		}
		if parsed.BlockHeader.RawData.Number == 0 {
			return 0, fmt.Errorf("getnowblock: no block number in response (%s)", truncate(string(body), 200))
		}
		return parsed.BlockHeader.RawData.Number, nil
	case config.ChainTypeSolana:
		raw, err := callJSONRPC(ctx, client, url, t.Headers, "getSlot", nil)
		if err != nil {
			return 0, err
		}
		var slot uint64
		if err := json.Unmarshal(raw, &slot); err != nil {
			return 0, fmt.Errorf("getSlot: unexpected result %s", truncate(string(raw), 100))
		}
		return slot, nil
	default:
		raw, err := callJSONRPC(ctx, client, url, t.Headers, "eth_blockNumber", nil)
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
