package chaintype

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Horizon is the registered name of the Stellar Horizon chain type. It is the
// one place the string literal lives: internal/config only declares constants
// for the types the gateway shipped with, so the fake node and the testnet
// checks build their config.ChainType from this.
const Horizon = "horizon"

func init() {
	Register(Spec{
		Name:        Horizon,
		PassThrough: true,
		Head:        horizonHead,
		Doc:         "Stellar Horizon REST API; the health check is `GET /` (`history_latest_ledger`)",
	})
}

// horizonRoot is the part of Horizon's root document the health check reads.
//
// history_latest_ledger is the last ledger Horizon has ingested into its own
// history database, which is what its endpoints can actually answer about. It
// is deliberately not core_latest_ledger: an instance whose stellar-core is at
// the head while its ingestion is stuck answers "fine" with stale data, and
// that is exactly what the block-lag check must catch.
type horizonRoot struct {
	HistoryLatestLedger *uint64 `json:"history_latest_ledger"`
}

// horizonHead reads the head of a Stellar network from Horizon's root document.
// Horizon reports client and server errors with a status code and an
// application/problem+json body, so a non-200 is already a failed check.
func horizonHead(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error) {
	body, err := getJSON(ctx, client, baseURL, headers)
	if err != nil {
		return 0, err
	}
	var parsed horizonRoot
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("horizon root: invalid response: %w (%s)", err, truncate(string(body), 200))
	}
	if parsed.HistoryLatestLedger == nil {
		return 0, fmt.Errorf("horizon root: no history_latest_ledger in response (%s)", truncate(string(body), 200))
	}
	if *parsed.HistoryLatestLedger == 0 {
		return 0, fmt.Errorf("horizon root: history_latest_ledger is 0, nothing ingested yet")
	}
	return *parsed.HistoryLatestLedger, nil
}
