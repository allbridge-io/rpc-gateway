package fakenode

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
)

func init() {
	RegisterSim(config.ChainTypeTron, tronSim)
}

// tronSim mimics the Tron HTTP API: /wallet/getnowblock returns a block,
// /jsonrpc speaks the EVM dialect, and any other path echoes the request so
// tests can see what reached the node. Tron reports most failures with HTTP 200
// and an "Error" field or a result code, which Behavior can produce for any path.
func tronSim(n *Node, r *http.Request, b Behavior, rpcMethod, body string) (any, bool) {
	switch {
	case b.TronError != "":
		return map[string]any{"Error": b.TronError}, true
	case b.TronResultCode != "":
		return map[string]any{"result": map[string]any{
			"code": b.TronResultCode, "message": hex.EncodeToString([]byte(b.TronResultCode)),
		}}, true
	}

	path := r.URL.Path
	switch {
	case path == "/jsonrpc":
		result, ok := evmResult(rpcMethod, b)
		if !ok {
			return nil, false // generic JSON-RPC echo
		}
		return RPCResult(body, result), true
	// A suffix match, so a target URL with a base path (a provider prefix or an
	// API key path) is simulated like the real node behind it.
	case strings.HasSuffix(path, "/wallet/getnowblock"), strings.HasSuffix(path, "/walletsolidity/getnowblock"):
		return map[string]any{
			"blockID":      fmt.Sprintf("%064x", b.Block),
			"block_header": map[string]any{"raw_data": map[string]any{"number": b.Block, "timestamp": time.Now().UnixMilli()}},
		}, true
	default:
		return n.Echo(r, body), true
	}
}
