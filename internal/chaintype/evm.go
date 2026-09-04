package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func init() {
	Register(Spec{
		Name: "evm",
		Head: evmHead,
		Doc:  "any EVM network; the health check is `eth_blockNumber`",
	})
}

// evmHead reads the head of an EVM chain with eth_blockNumber, whose result is
// a 0x-prefixed quantity.
func evmHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	raw, err := callJSONRPC(ctx, client, baseURL, headers, "eth_blockNumber", nil)
	if err != nil {
		return 0, err
	}
	var hex string
	if err := json.Unmarshal(raw, &hex); err != nil {
		return 0, fmt.Errorf("eth_blockNumber: unexpected result %s", truncate(string(raw), 100))
	}
	return parseHexUint64(hex)
}
