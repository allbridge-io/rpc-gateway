package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func init() {
	Register(Spec{
		Name: Solana,
		Head: solanaHead,
		Doc:  "Solana JSON-RPC; the health check is `getSlot`",
	})
}

// solanaHead reads the current slot, which Solana returns as a plain number.
func solanaHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	raw, err := callJSONRPC(ctx, client, baseURL, headers, "getSlot", []any{})
	if err != nil {
		return 0, err
	}
	var slot uint64
	if err := json.Unmarshal(raw, &slot); err != nil {
		return 0, fmt.Errorf("getSlot: unexpected result %s", truncate(string(raw), 100))
	}
	return slot, nil
}
