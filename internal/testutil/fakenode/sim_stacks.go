package fakenode

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// ChainTypeStacks is the chains.X.type value of the Stacks chain type.
// internal/config only keeps constants for the types it names itself, so the
// Stacks tests take the value from here.
const ChainTypeStacks config.ChainType = "stacks"

// stacksTestnetNetworkID is what a Stacks testnet node reports in /v2/info
// (0x80000000); mainnet reports 1.
const stacksTestnetNetworkID = 2147483648

func init() {
	RegisterSim(ChainTypeStacks, stacksSim)
}

// stacksSim mimics the Hiro Stacks API, which serves both the node's /v2/*
// endpoints and Hiro's own /extended/* REST API. Only /v2/info is simulated —
// it is the health check — and every other path echoes the request so tests can
// see what reached the node.
//
// Hiro reports every failure with a real HTTP status code and a JSON body
// ({"statusCode":400,"error":"Bad Request","message":...}), which
// Behavior.HTTPStatus together with Behavior.RawBody reproduces for any path,
// so this simulation needs no error knob of its own.
func stacksSim(n *Node, r *http.Request, b Behavior, _ string, body string) (any, bool) {
	// A suffix match, so a target URL with a base path (a provider prefix or an
	// API key path) is simulated like the real node behind it.
	if strings.HasSuffix(r.URL.Path, "/v2/info") {
		return map[string]any{
			"peer_version":      4207599120,
			"server_version":    "stacks-node 4.0.1 (fakenode)",
			"network_id":        stacksTestnetNetworkID,
			"parent_network_id": 3669344250,
			"stacks_tip_height": b.Block,
			"stacks_tip":        fmt.Sprintf("%064x", b.Block),
			"burn_block_height": b.Block / 20, // one burn block per Bitcoin block
			"tenure_height":     b.Block,
			"is_fully_synced":   true,
		}, true
	}
	return n.Echo(r, body), true
}
