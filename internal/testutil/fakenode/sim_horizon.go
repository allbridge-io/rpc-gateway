package fakenode

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// config.ChainTypeHorizon is the config value of the Stellar Horizon chain type.
// internal/config only declares constants for the types the gateway shipped
// with, so tests of the newer types take theirs from the fake node.

func init() {
	RegisterSim(config.ChainTypeHorizon, horizonSim)
}

// horizonSim mimics the Stellar Horizon REST API: the root document carries the
// ingested ledger the health check reads, /ledgers answers with a HAL
// collection, and any other path echoes the request so pass-through tests can
// see what reached the node.
func horizonSim(n *Node, r *http.Request, b Behavior, _ string, body string) (any, bool) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		return horizonRootDoc(b.Block), true
	// A suffix match, so a target URL with a base path (a provider prefix) is
	// simulated like the real Horizon behind it.
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ledgers"):
		return horizonLedgersDoc(b.Block), true
	default:
		return n.Echo(r, body), true
	}
}

// HorizonProblem returns the Behavior of a Horizon node answering with the
// error document it uses for every client and server error. The real one is
// served as application/problem+json; the fake node writes application/json,
// because only the status code and the body reach the gateway's decisions.
func HorizonProblem(status int, kind, title, detail string) Behavior {
	return Behavior{HTTPStatus: status, RawBody: string(mustJSON(map[string]any{
		"type":   "https://stellar.org/horizon-errors/" + kind,
		"title":  title,
		"status": status,
		"detail": detail,
	}))}
}

// horizonRootDoc is Horizon's root document, trimmed to the fields that matter.
// ingest_latest_ledger and core_latest_ledger follow history_latest_ledger:
// a node that is behind is behind everywhere, and the health check reads the
// history one on purpose.
func horizonRootDoc(ledger uint64) map[string]any {
	return map[string]any{
		"_links": map[string]any{
			"ledgers": map[string]any{"href": "/ledgers{?cursor,limit,order}", "templated": true},
		},
		"horizon_version":                 "28.0.1-fakenode",
		"core_version":                    "stellar-core 28.0.1 (fakenode)",
		"ingest_latest_ledger":            ledger,
		"history_latest_ledger":           ledger,
		"history_latest_ledger_closed_at": time.Now().UTC().Format(time.RFC3339),
		"history_elder_ledger":            1,
		"core_latest_ledger":              ledger,
		"network_passphrase":              "Test SDF Network ; September 2015",
		"current_protocol_version":        28,
		"supported_protocol_version":      28,
	}
}

// horizonLedgersDoc is a one-record /ledgers collection in Horizon's HAL shape.
func horizonLedgersDoc(ledger uint64) map[string]any {
	return map[string]any{
		"_links": map[string]any{
			"self": map[string]any{"href": "/ledgers?cursor=&limit=1&order=desc"},
		},
		"_embedded": map[string]any{
			"records": []any{map[string]any{
				"id":           fmt.Sprintf("%064x", ledger),
				"paging_token": strconv.FormatUint(ledger<<32, 10),
				"hash":         fmt.Sprintf("%064x", ledger),
				"sequence":     ledger,
				"closed_at":    time.Now().UTC().Format(time.RFC3339),
			}},
		},
	}
}
