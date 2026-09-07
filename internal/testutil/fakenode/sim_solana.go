package fakenode

import (
	"net/http"

	"github.com/0xProject/rpc-gateway/internal/config"
)

func init() {
	RegisterSim(config.ChainTypeSolana, func(_ *Node, _ *http.Request, b Behavior, rpcMethod, body string) (any, bool) {
		switch rpcMethod {
		case "getSlot":
			return RPCResult(body, b.Block), true
		case "getHealth":
			return RPCResult(body, "ok"), true
		default:
			return nil, false
		}
	})
}
