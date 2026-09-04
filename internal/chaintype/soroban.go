package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func init() {
	Register(Spec{
		Name: "soroban",
		Head: sorobanHead,
		Doc:  "Stellar Soroban RPC; the health check is `getHealth`",
	})
}

// sorobanHealthResult is the result of Soroban RPC's getHealth.
type sorobanHealthResult struct {
	Status       string `json:"status"`
	LatestLedger uint64 `json:"latestLedger"`
}

// sorobanHead reads the head of a Stellar network from a Soroban RPC server.
//
// The check is getHealth and not getLatestLedger: since protocol 23 the latter
// answers with the whole ledger close meta ("metadataXdr", 100-400 KiB on
// testnet and more on mainnet), which every check of every target would
// download and which would eventually run into the response size limit.
// getHealth answers in a few hundred bytes with the same ledger sequence plus
// the server's verdict on itself — an RPC that is out of sync or still
// catching up reports a status other than "healthy" — so it is both the
// cheaper and the stricter check.
func sorobanHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	raw, err := sorobanCall(ctx, client, baseURL, headers, "getHealth")
	if err != nil {
		return 0, err
	}
	var h sorobanHealthResult
	if err := json.Unmarshal(raw, &h); err != nil {
		return 0, fmt.Errorf("getHealth: unexpected result %s", truncate(string(raw), 200))
	}
	switch {
	case h.Status == "":
		return 0, fmt.Errorf("getHealth: no status in response (%s)", truncate(string(raw), 200))
	case h.Status != "healthy":
		return 0, fmt.Errorf("getHealth: status %q", h.Status)
	case h.LatestLedger == 0:
		return 0, fmt.Errorf("getHealth: no latestLedger in response (%s)", truncate(string(raw), 200))
	}
	return h.LatestLedger, nil
}

// sorobanCall performs one JSON-RPC 2.0 call with no "params" member at all.
//
// Soroban RPC unmarshals params into a per-method request struct, so the empty
// array a generic JSON-RPC client sends for "no arguments" (what the shared
// callJSONRPC does) is answered with -32602 "invalid parameters: json: cannot
// unmarshal array into Go value of type protocol.GetHealthRequest". Omitting
// the member is what its own SDKs do and what the server accepts.
func sorobanCall(ctx context.Context, client *http.Client, url string, headers map[string]string, method string) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method})
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
