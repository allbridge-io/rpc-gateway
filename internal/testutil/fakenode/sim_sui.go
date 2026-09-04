package fakenode

import (
	"net/http"
	"strconv"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// chainTypeSui is the config value of the Sui chain type. internal/config keeps
// constants only for the types that existed before the registry, so the name is
// spelled out here (and validated against the registry at config load).
const chainTypeSui = config.ChainType("sui")

// suiTestnetChainIdentifier is what the real testnet answers to
// sui_getChainIdentifier: the first four bytes of the genesis checkpoint digest.
const suiTestnetChainIdentifier = "4c78adac"

func init() {
	RegisterSim(chainTypeSui, suiSim)
}

// suiSim mimics the Sui JSON-RPC API. Every u64 is a decimal string, the way
// the real node encodes it, so a Head that forgets to unquote fails here for
// the same reason it would fail against a real provider. An unknown object is
// reported inside the *result*, not as a JSON-RPC error, which is why it must
// reach the client untouched.
func suiSim(_ *Node, _ *http.Request, b Behavior, rpcMethod, body string) (any, bool) {
	switch rpcMethod {
	case "sui_getLatestCheckpointSequenceNumber":
		return RPCResult(body, strconv.FormatUint(b.Block, 10)), true
	case "sui_getChainIdentifier":
		return RPCResult(body, suiTestnetChainIdentifier), true
	case "suix_getReferenceGasPrice":
		return RPCResult(body, "1000"), true
	case "sui_getObject":
		return RPCResult(body, map[string]any{"error": map[string]any{
			"code": "notExists", "object_id": "0x00000000000000000000000000000000000000000000000000000000deadbeef",
		}}), true
	default:
		return nil, false
	}
}
