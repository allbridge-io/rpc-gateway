package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func init() {
	Register(Spec{
		Name: Soroban,
		Head: sorobanHead,
		Doc:  "Stellar Soroban RPC; the health check is `getHealth`",
	})
}

// sorobanHealthResult is the result of Soroban RPC's getHealth.
type sorobanHealthResult struct {
	Status       string `json:"status"`
	LatestLedger uint64 `json:"latestLedger"`
}

// sorobanHead reads the head of a Stellar network from a Soroban RPC server.
//
// The check is getHealth and not getLatestLedger: since protocol 23 the latter
// answers with the whole ledger close meta ("metadataXdr", 100-400 KiB on
// testnet and more on mainnet), which every check of every target would
// download and which would eventually run into the response size limit.
// getHealth answers in a few hundred bytes with the same ledger sequence plus
// the server's verdict on itself — an RPC that is out of sync or still
// catching up reports a status other than "healthy" — so it is both the
// cheaper and the stricter check.
func sorobanHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	raw, err := callJSONRPC(ctx, client, baseURL, headers, "getHealth", nil)
	if err != nil {
		return 0, err
	}
	var h sorobanHealthResult
	if err := json.Unmarshal(raw, &h); err != nil {
		return 0, fmt.Errorf("getHealth: unexpected result %s", truncate(string(raw), 200))
	}
	switch {
	case h.Status == "":
		return 0, fmt.Errorf("getHealth: no status in response (%s)", truncate(string(raw), 200))
	case h.Status != "healthy":
		return 0, fmt.Errorf("getHealth: status %q", h.Status)
	case h.LatestLedger == 0:
		return 0, fmt.Errorf("getHealth: no latestLedger in response (%s)", truncate(string(raw), 200))
	}
	return h.LatestLedger, nil
}
