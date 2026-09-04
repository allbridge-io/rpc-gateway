package fakenode

import (
	"fmt"
	"net/http"

	"github.com/0xProject/rpc-gateway/internal/config"
)

func init() {
	RegisterSim(config.ChainTypeEVM, func(_ *Node, _ *http.Request, b Behavior, rpcMethod, body string) (any, bool) {
		result, ok := evmResult(rpcMethod, b)
		if !ok {
			return nil, false
		}
		return RPCResult(body, result), true
	})
}

// evmResult answers the JSON-RPC methods of an EVM node. Tron's /jsonrpc
// endpoint speaks the same dialect and reuses it.
func evmResult(rpcMethod string, b Behavior) (any, bool) {
	switch rpcMethod {
	case "eth_blockNumber":
		return fmt.Sprintf("0x%x", b.Block), true
	case "eth_chainId":
		if b.ChainID == "" {
			return "0x1", true
		}
		return b.ChainID, true
	default:
		return nil, false
	}
}
