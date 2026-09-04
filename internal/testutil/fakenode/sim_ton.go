package fakenode

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// config.ChainTypeTON is the chain type of a TON node. It lives here rather than in
// internal/config because a chain type is meant to be added as new files only;
// fakenode is the one package every test of the type already imports.

func init() {
	RegisterSim(config.ChainTypeTON, tonSim)
}

// tonSim mimics toncenter: GET /api/v3/masterchainInfo reports the head,
// POST /api/v2/jsonRPC answers getMasterchainInfo in toncenter's
// {"ok":true,"result":...} envelope, and any other path echoes the request so
// tests can see what reached the node.
//
// toncenter reports failures as {"error":"..."} with a 4xx/5xx status, which is
// Behavior{HTTPStatus: 422, RawBody: `{"error":"..."}`} — no extra knob needed.
func tonSim(n *Node, r *http.Request, b Behavior, rpcMethod, body string) (any, bool) {
	path := r.URL.Path
	switch {
	// A suffix match, so a target URL with a base path (a provider prefix or an
	// API key path) is simulated like the real node behind it.
	case strings.HasSuffix(path, "/api/v3/masterchainInfo"):
		return map[string]any{"last": tonBlockID(b.Block), "first": tonBlockID(1)}, true
	case strings.HasSuffix(path, "/api/v2/jsonRPC"):
		if rpcMethod != "getMasterchainInfo" {
			return nil, false // generic JSON-RPC echo
		}
		return map[string]any{
			"ok": true,
			"result": map[string]any{
				"@type": "blocks.masterchainInfo",
				"last":  tonBlockID(b.Block),
			},
			"@extra": fmt.Sprintf("%d:0:0.1", time.Now().Unix()),
		}, true
	default:
		return n.Echo(r, body), true
	}
}

// tonBlockID is a ton.blockIdExt of the masterchain at the given seqno.
func tonBlockID(seqno uint64) map[string]any {
	return map[string]any{
		"workchain": -1,
		"shard":     "8000000000000000",
		"seqno":     seqno,
		"root_hash": fmt.Sprintf("%064x", seqno),
		"file_hash": fmt.Sprintf("%064x", seqno),
	}
}
