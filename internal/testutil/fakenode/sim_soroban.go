package fakenode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// ChainTypeSoroban is the config value of the Stellar Soroban RPC chain type.
// internal/config only keeps constants for the types its own tests name, and
// the chain type registry is the source of truth, so the simulation declares
// the one it answers for.
const ChainTypeSoroban config.ChainType = "soroban"

func init() {
	RegisterSim(ChainTypeSoroban, sorobanSim)
}

// sorobanLedgerRetentionWindow is what soroban-rpc reports on testnet (7 days
// of ledgers at ~5s each); the exact value does not matter, its presence does.
const sorobanLedgerRetentionWindow = 120960

// sorobanSim mimics a Soroban RPC server: getHealth, getLatestLedger and
// getNetwork answer with the shapes the real testnet server returns, and any
// request whose "params" member is not an object is rejected exactly like the
// real server rejects it. Everything else falls back to the generic JSON-RPC
// echo, and the generic knobs of Behavior (HTTPStatus, RawBody, RPCError,
// Drop, Hang, Latency) cover every failure the API reports as plain JSON-RPC.
func sorobanSim(_ *Node, _ *http.Request, b Behavior, rpcMethod, body string) (any, bool) {
	// Soroban RPC unmarshals "params" into a per-method request struct, so the
	// empty array a generic JSON-RPC client sends for "no arguments" is an
	// error. Reproducing it here means a health check or a proxy test built on
	// the wrong shape fails offline for the same reason it fails on testnet.
	if params, ok := sorobanParams(body); ok && !strings.HasPrefix(params, "{") {
		return RPCErrorBody(body, -32602, fmt.Sprintf(
			"invalid parameters: json: cannot unmarshal %s into Go value of type protocol.Request", sorobanJSONKind(params))), true
	}

	switch rpcMethod {
	case "getHealth":
		return RPCResult(body, map[string]any{
			"status":                "healthy",
			"latestLedger":          b.Block,
			"latestLedgerCloseTime": "1788532032",
			"oldestLedger":          sorobanOldestLedger(b.Block),
			"ledgerRetentionWindow": sorobanLedgerRetentionWindow,
		}), true
	case "getLatestLedger":
		return RPCResult(body, map[string]any{
			"id":              fmt.Sprintf("%064x", b.Block),
			"protocolVersion": 23,
			"sequence":        b.Block,
			"closeTime":       "1788532032",
		}), true
	case "getNetwork":
		return RPCResult(body, map[string]any{
			"friendbotUrl":    "https://friendbot.stellar.org/",
			"passphrase":      "Test SDF Network ; September 2015",
			"protocolVersion": 23,
		}), true
	default:
		return nil, false
	}
}

// sorobanParams returns the raw "params" member of a JSON-RPC body, trimmed.
// A missing or null member reports false: that is the shape the server wants.
func sorobanParams(body string) (string, bool) {
	var req struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil || len(req.Params) == 0 {
		return "", false
	}
	params := strings.TrimSpace(string(req.Params))
	if params == "null" {
		return "", false
	}
	return params, true
}

// sorobanJSONKind names the JSON kind of a raw value the way encoding/json
// does in its unmarshal errors, so the fake error reads like the real one.
func sorobanJSONKind(raw string) string {
	switch {
	case strings.HasPrefix(raw, "["):
		return "array"
	case strings.HasPrefix(raw, `"`):
		return "string"
	default:
		return "number"
	}
}

// sorobanOldestLedger is the start of the retention window, never below 1.
func sorobanOldestLedger(latest uint64) uint64 {
	if latest <= sorobanLedgerRetentionWindow {
		return 1
	}
	return latest - sorobanLedgerRetentionWindow
}
