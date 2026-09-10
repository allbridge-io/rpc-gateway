package chaintype

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
)

const (
	// healthCheckUserAgent identifies the gateway's own calls in provider logs.
	healthCheckUserAgent = "rpc-gateway-health-check"
	// maxResponseBytes bounds a health check response: 1 MiB is plenty for a
	// block number, and a provider streaming garbage cannot exhaust memory.
	maxResponseBytes = 1 << 20
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	// Params is omitted when nil: some servers (Soroban RPC) reject "params": []
	// for parameterless methods, others (EVM, Solana) are happy either way.
	Params any `json:"params,omitempty"`
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
	return do(client, req, headers)
}

// getJSON performs a GET with the target's extra headers and returns the
// response body. A non-200 status is an error.
func getJSON(ctx context.Context, client *http.Client, url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	return do(client, req, headers)
}

// do sends the request with the health-check user agent and the target's
// headers, and reads a size-limited body. Only HTTP 200 is a success.
func do(client *http.Client, req *http.Request, headers map[string]string) ([]byte, error) {
	req.Header.Set("User-Agent", healthCheckUserAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, withoutURLSecrets(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, describeBody(body))
	}
	return body, nil
}

// callJSONRPC performs one JSON-RPC 2.0 call over HTTP and returns the raw
// result. Non-200 responses, malformed bodies and JSON-RPC errors are errors.
//
// params is sent as given: pass []any{} for an explicit empty array, or nil
// to leave the field out entirely.
func callJSONRPC(ctx context.Context, client *http.Client, url string, headers map[string]string, method string, params any) (json.RawMessage, error) {
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

// JoinURLPath appends a sub-path to a base URL that may already have a path.
func JoinURLPath(base, sub string) string {
	if sub == "" || sub == "/" {
		return base
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(sub, "/")
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

// truncate shortens a body quoted in an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// describeBody compacts a non-200 response body for an error message. A proxy
// error page (nginx 503 & co.) is worth one line, not eight: it is reduced to
// its <title>. Anything else keeps its text, with whitespace runs collapsed so
// a pretty-printed JSON error stays on a single line, and is truncated.
func describeBody(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "empty body"
	}
	if s[0] == '<' {
		lower := strings.ToLower(s)
		if open := strings.Index(lower, "<title>"); open >= 0 {
			rest := lower[open+len("<title>"):]
			// ToLower can change byte lengths on non-ASCII text, so the
			// indices found in lower are only trusted inside s's bounds.
			if closeAt := strings.Index(rest, "</title>"); closeAt >= 0 && open+len("<title>")+closeAt <= len(s) {
				start := open + len("<title>")
				if title := strings.TrimSpace(s[start : start+closeAt]); title != "" {
					return "html page: " + title
				}
			}
		}
		return "html page"
	}
	return truncate(strings.Join(strings.Fields(s), " "), 200)
}

// withoutURLSecrets strips path, query and userinfo from the URL that
// net/http embeds in *url.Error, keeping scheme and host. Target URLs often
// carry API keys (".../v2/<key>", "?api-key=..."); the error text ends up in
// /status and in logs, and must never repeat them. The wrapped cause is kept
// so errors.Is(err, context.DeadlineExceeded) and friends still work.
func withoutURLSecrets(err error) error {
	var ue *neturl.Error
	if !errors.As(err, &ue) {
		return err
	}
	safe := "<url>"
	if u, perr := neturl.Parse(ue.URL); perr == nil && u.Host != "" {
		safe = u.Scheme + "://" + u.Host
	}
	return &neturl.Error{Op: ue.Op, URL: safe, Err: ue.Err}
}
