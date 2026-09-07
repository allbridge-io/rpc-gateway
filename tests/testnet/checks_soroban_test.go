//go:build testnet

package testnet

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
)

const chainTypeSoroban config.ChainType = "soroban"

func init() {
	registerChecks(chainTypeSoroban, typeChecks{
		Verify: testSoroban,
		Probe: func(t *testing.T, e *env, key string) *http.Response {
			_, resp := sorobanCall(t, e, "/"+key, "getHealth", nil)
			return resp
		},
	})
}

// sorobanCall performs one Soroban JSON-RPC call through the gateway and fails
// the test on any error. It cannot use env.rpc: that helper always sends
// "params" as an array, which Soroban RPC rejects with -32602 (it unmarshals
// params into a per-method request struct). Passing nil omits the member, the
// way the Stellar SDKs do it.
func sorobanCall(t *testing.T, e *env, path, method string, params map[string]any) (json.RawMessage, *http.Response) {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, data := e.do(t, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: HTTP %d %s", path, method, resp.StatusCode, truncate(data))
	}
	var parsed rpcResp
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("%s %s: not JSON-RPC: %s", path, method, truncate(data))
	}
	if parsed.Error != nil {
		t.Fatalf("%s %s: json-rpc error %d: %s", path, method, parsed.Error.Code, parsed.Error.Message)
	}
	if provider := resp.Header.Get("X-Rpc-Provider"); provider == "" || provider == deadTargetName {
		t.Fatalf("%s %s: served by %q", path, method, provider)
	}
	return parsed.Result, resp
}

func testSoroban(t *testing.T, e *env, key string, _ config.Chain) {
	path := "/" + key

	t.Run("getHealth", func(t *testing.T) {
		raw, resp := sorobanCall(t, e, path, "getHealth", nil)
		var h struct {
			Status       string `json:"status"`
			LatestLedger uint64 `json:"latestLedger"`
			OldestLedger uint64 `json:"oldestLedger"`
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatalf("getHealth: %v (%s)", err, raw)
		}
		t.Logf("%s status=%s latestLedger=%d oldestLedger=%d provider=%s",
			key, h.Status, h.LatestLedger, h.OldestLedger, resp.Header.Get("X-Rpc-Provider"))
		if h.Status != "healthy" {
			t.Errorf("status = %q, want healthy", h.Status)
		}
		if h.LatestLedger == 0 {
			t.Error("latestLedger is zero")
		}
	})

	t.Run("getNetwork", func(t *testing.T) {
		raw, _ := sorobanCall(t, e, path, "getNetwork", nil)
		var n struct {
			Passphrase      string `json:"passphrase"`
			ProtocolVersion int    `json:"protocolVersion"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatalf("getNetwork: %v (%s)", err, raw)
		}
		t.Logf("%s passphrase=%q protocolVersion=%d", key, n.Passphrase, n.ProtocolVersion)
		if !strings.Contains(n.Passphrase, "Test SDF Network") {
			t.Errorf("passphrase %q: %s is not pointed at the Stellar testnet", n.Passphrase, key)
		}
		if n.ProtocolVersion == 0 {
			t.Error("protocolVersion is zero")
		}
	})

	t.Run("getLatestLedger", func(t *testing.T) {
		raw, _ := sorobanCall(t, e, path, "getLatestLedger", nil)
		var l struct {
			ID       string `json:"id"`
			Sequence uint64 `json:"sequence"`
		}
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatalf("getLatestLedger: %v", err)
		}
		if l.Sequence == 0 || len(l.ID) != 64 {
			t.Fatalf("unexpected ledger id=%q sequence=%d", l.ID, l.Sequence)
		}

		// The sequence a client sees and the one the health check recorded must
		// describe the same chain head; ledgers close every ~5s.
		var checked uint64
		for _, s := range e.gw.Chain(key).Manager.Status() {
			if s.BlockNumber > checked {
				checked = s.BlockNumber
			}
		}
		if diff := absDiff(l.Sequence, checked); diff > 20 {
			t.Errorf("ledger via proxy %d differs from the health-check ledger %d by %d", l.Sequence, checked, diff)
		}
	})

	t.Run("client error passed through", func(t *testing.T) {
		// A malformed ledger key is the client's mistake: Soroban RPC answers
		// HTTP 200 with a JSON-RPC error, which must reach the client as-is
		// instead of being turned into a reroute or a 503.
		before := len(e.rec.Of(events.KindRerouted, ""))
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "getLedgerEntries",
			"params": map[string]any{"keys": []string{"not-valid-xdr"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, data := e.do(t, http.MethodPost, path, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected HTTP 200 with a JSON-RPC error, got %d %s", resp.StatusCode, truncate(data))
		}
		var parsed rpcResp
		if err := json.Unmarshal(data, &parsed); err != nil || parsed.Error == nil {
			t.Fatalf("expected a JSON-RPC error, got %s", truncate(data))
		}
		t.Logf("%s passed through: %d %s", key, parsed.Error.Code, parsed.Error.Message)
		if after := len(e.rec.Of(events.KindRerouted, "")); after != before {
			t.Errorf("a client error must not cause a reroute (%d new)", after-before)
		}
	})

	t.Run("sub-path is not a route", func(t *testing.T) {
		resp, data := e.do(t, http.MethodPost, path+"/getHealth", []byte(`{"jsonrpc":"2.0","id":1,"method":"getHealth"}`))
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(data), "single endpoint") {
			t.Errorf("soroban serves one endpoint; got HTTP %d %s", resp.StatusCode, truncate(data))
		}
	})

	t.Run("repeated requests stay on healthy targets", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			sorobanCall(t, e, path, "getHealth", nil)
		}
	})
}
