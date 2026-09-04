//go:build testnet

package testnet

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/chaintype"
	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
)

// horizonUnfundedAccount is a well-formed Stellar account id derived from a
// fixed string, so it is valid strkey but has never been created on testnet:
// Horizon answers 404 for it, which is a client error, not a broken provider.
const horizonUnfundedAccount = "GADMTC3BAGNJRCSRNMS5DDE4MSZFCGZ464CFINXUB7UIBPLXBYWXLS27"

func init() {
	registerChecks(config.ChainType(chaintype.Horizon), typeChecks{
		Verify: testHorizon,
		Probe: func(t *testing.T, e *env, key string) *http.Response {
			// The root document is Horizon's cheapest endpoint and the one the
			// health check uses.
			resp, data := e.do(t, http.MethodGet, "/"+key, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
			}
			return resp
		},
	})
}

func testHorizon(t *testing.T, e *env, key string, _ config.Chain) {
	path := "/" + key
	var rootLedger uint64

	t.Run("root document", func(t *testing.T) {
		resp, data := e.do(t, http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		var root struct {
			HistoryLatestLedger uint64 `json:"history_latest_ledger"`
			CoreLatestLedger    uint64 `json:"core_latest_ledger"`
			NetworkPassphrase   string `json:"network_passphrase"`
			HorizonVersion      string `json:"horizon_version"`
		}
		if err := json.Unmarshal(data, &root); err != nil {
			t.Fatalf("root is not JSON: %s", truncate(data))
		}
		if root.HistoryLatestLedger == 0 {
			t.Fatalf("no history_latest_ledger in %s", truncate(data))
		}
		rootLedger = root.HistoryLatestLedger
		t.Logf("%s ledger=%d core=%d %q horizon=%s provider=%s", key, root.HistoryLatestLedger, root.CoreLatestLedger,
			root.NetworkPassphrase, root.HorizonVersion, resp.Header.Get("X-Rpc-Provider"))

		// The key must really point at a test network, not at pubnet.
		if !strings.Contains(root.NetworkPassphrase, "Test SDF Network") {
			t.Errorf("network passphrase %q is not the testnet one", root.NetworkPassphrase)
		}

		// The same number the health check reads: the two must agree.
		var checked uint64
		for _, s := range e.gw.Chain(key).Manager.Status() {
			if s.BlockNumber > checked {
				checked = s.BlockNumber
			}
		}
		if diff := absDiff(rootLedger, checked); diff > 20 {
			t.Errorf("ledger via proxy %d differs from health-check ledger %d by %d", rootLedger, checked, diff)
		}
	})

	t.Run("ledgers collection", func(t *testing.T) {
		resp, data := e.do(t, http.MethodGet, path+"/ledgers?order=desc&limit=1", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		var page struct {
			Embedded struct {
				Records []struct {
					Sequence uint64 `json:"sequence"`
					ClosedAt string `json:"closed_at"`
				} `json:"records"`
			} `json:"_embedded"`
		}
		if err := json.Unmarshal(data, &page); err != nil || len(page.Embedded.Records) != 1 {
			t.Fatalf("no _embedded.records[0] in %s", truncate(data))
		}
		seq := page.Embedded.Records[0].Sequence
		if seq == 0 {
			t.Fatalf("ledger sequence is zero: %s", truncate(data))
		}
		t.Logf("latest ledger %d closed at %s", seq, page.Embedded.Records[0].ClosedAt)
		if rootLedger != 0 && absDiff(seq, rootLedger) > 20 {
			t.Errorf("/ledgers says %d, the root document said %d", seq, rootLedger)
		}
	})

	t.Run("client errors are passed through", func(t *testing.T) {
		before := horizonReroutes(e, key)
		cases := []struct {
			name       string
			path       string
			wantStatus int
			wantType   string
		}{
			{"malformed account id", path + "/accounts/GINVALID", http.StatusBadRequest, "bad_request"},
			{"unknown account", path + "/accounts/" + horizonUnfundedAccount, http.StatusNotFound, "not_found"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				resp, data := e.do(t, http.MethodGet, tc.path, nil)
				if resp.StatusCode != tc.wantStatus {
					t.Fatalf("HTTP %d, want %d: %s", resp.StatusCode, tc.wantStatus, truncate(data))
				}
				var problem struct {
					Type   string `json:"type"`
					Title  string `json:"title"`
					Status int    `json:"status"`
				}
				if err := json.Unmarshal(data, &problem); err != nil {
					t.Fatalf("not Horizon's problem document: %s", truncate(data))
				}
				if !strings.HasSuffix(problem.Type, tc.wantType) || problem.Status != tc.wantStatus {
					t.Errorf("unexpected problem document: %s", truncate(data))
				}
				if provider := resp.Header.Get("X-Rpc-Provider"); provider == "" || provider == deadTargetName {
					t.Errorf("served by %q", provider)
				}
			})
		}
		if after := horizonReroutes(e, key); after != before {
			t.Errorf("a client error must not cause a reroute: %d new reroutes", after-before)
		}
	})

	t.Run("repeated requests stay on healthy targets", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			resp, data := e.do(t, http.MethodGet, path+"/ledgers?limit=1", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
			}
			if provider := resp.Header.Get("X-Rpc-Provider"); provider == deadTargetName {
				t.Fatalf("request %d served by the dead target", i)
			}
		}
	})
}

// horizonReroutes counts the reroutes recorded for one chain.
func horizonReroutes(e *env, key string) int {
	n := 0
	for _, ev := range e.rec.Of(events.KindRerouted, "") {
		if ev.Chain == key {
			n++
		}
	}
	return n
}
