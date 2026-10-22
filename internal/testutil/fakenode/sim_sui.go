package fakenode

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// suiTestnetChainIdentifier is what the real testnet GraphQL answers to
// { chainIdentifier }: the Base58 digest of the genesis checkpoint.
const suiTestnetChainIdentifier = "69WiPg3DAQiwdxfncX6wYQ2siKwAe6L9BZthQea3JNMD"

func init() {
	RegisterSim(config.ChainTypeSui, suiSim)
}

// suiSim mimics the Sui GraphQL RPC served at /graphql under the provider's
// base URL. The latest checkpoint (the health check) and the chain identifier
// are simulated; the checkpoint number is a JSON number, the way the UInt53
// scalar is encoded, so a Head that expects the JSON-RPC decimal string fails
// here for the same reason it would fail against a real provider.
//
// Any other query and any other path echoes the request so tests can see what
// reached the node. A GraphQL error (HTTP 200, "data": null, "errors": [...])
// is produced with Behavior.RawBody.
func suiSim(n *Node, r *http.Request, b Behavior, _ string, body string) (any, bool) {
	// A suffix match, so a target URL with a base path (a provider token path)
	// is simulated like the real server behind it.
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/graphql") {
		return n.Echo(r, body), true
	}
	var req struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	data := map[string]any{}
	if strings.Contains(req.Query, "checkpoint") {
		data["checkpoint"] = map[string]any{"sequenceNumber": b.Block}
	}
	if strings.Contains(req.Query, "chainIdentifier") {
		data["chainIdentifier"] = suiTestnetChainIdentifier
	}
	if len(data) == 0 {
		return n.Echo(r, body), true
	}
	return map[string]any{"data": data}, true
}
