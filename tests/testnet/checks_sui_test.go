//go:build testnet

package testnet

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
)

// chainTypeSui is the config value of the Sui chain type.
const chainTypeSui = config.ChainType("sui")

func init() {
	registerChecks(chainTypeSui, typeChecks{
		Verify: func(t *testing.T, e *env, key string, _ config.Chain) { testSui(t, e, key) },
		Probe: func(t *testing.T, e *env, key string) *http.Response {
			_, resp := e.rpc(t, "/"+key, "sui_getLatestCheckpointSequenceNumber")
			return resp
		},
	})
}

// suiU64 decodes a Sui u64, which the API sends as a decimal string.
func suiU64(t *testing.T, raw json.RawMessage) uint64 {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("expected a decimal string, got %s", raw)
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("not a decimal u64: %s", s)
	}
	return v
}

func testSui(t *testing.T, e *env, key string) {
	path := "/" + key

	t.Run("sui_getChainIdentifier", func(t *testing.T) {
		raw, resp := e.rpc(t, path, "sui_getChainIdentifier")
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			t.Fatalf("expected a string, got %s", raw)
		}
		t.Logf("%s chainIdentifier=%s provider=%s", key, id, resp.Header.Get("X-Rpc-Provider"))
		// The identifier is the first four bytes of the genesis checkpoint
		// digest: eight hex characters, the same for every node of a network.
		if len(id) != 8 {
			t.Errorf("chain identifier %q is not 8 hex characters", id)
		}
		if _, err := strconv.ParseUint(id, 16, 64); err != nil {
			t.Errorf("chain identifier %q is not hexadecimal", id)
		}
	})

	t.Run("sui_getLatestCheckpointSequenceNumber", func(t *testing.T) {
		seq := suiU64(t, mustRaw(e.rpc(t, path, "sui_getLatestCheckpointSequenceNumber")))
		if seq == 0 {
			t.Fatal("checkpoint sequence number is zero")
		}
		var checked uint64
		for _, s := range e.gw.Chain(key).Manager.Status() {
			if s.BlockNumber > checked {
				checked = s.BlockNumber
			}
		}
		t.Logf("%s checkpoint=%d health-check checkpoint=%d", key, seq, checked)
		// Checkpoints come every ~250ms, so a health round some seconds old is
		// still within a few hundred of the live value.
		if diff := absDiff(seq, checked); diff > 2000 {
			t.Errorf("checkpoint via proxy %d differs from the health-check one %d by %d", seq, checked, diff)
		}
	})

	t.Run("suix_getReferenceGasPrice", func(t *testing.T) {
		if price := suiU64(t, mustRaw(e.rpc(t, path, "suix_getReferenceGasPrice"))); price == 0 {
			t.Error("reference gas price is zero")
		}
	})

	t.Run("client error passed through", func(t *testing.T) {
		// A malformed object id is the client's mistake: Sui answers HTTP 200
		// with a JSON-RPC error, which must reach the client as-is instead of
		// being turned into a 503 or rerouted to another provider.
		before := len(e.rec.Of(events.KindRerouted, ""))
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "sui_getObject", "params": []any{"not-an-object-id"},
		})
		resp, data := e.do(t, http.MethodPost, path, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		var parsed rpcResp
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("not JSON-RPC: %s", truncate(data))
		}
		if parsed.Error == nil {
			t.Fatalf("expected a JSON-RPC error for a malformed object id, got %s", truncate(data))
		}
		if !strings.Contains(strings.ToLower(parsed.Error.Message), "invalid params") {
			t.Errorf("unexpected error %d: %s", parsed.Error.Code, parsed.Error.Message)
		}
		if after := len(e.rec.Of(events.KindRerouted, "")); after != before {
			t.Errorf("a client error must not cause a reroute: %d new reroutes", after-before)
		}
	})

	t.Run("repeated requests stay on healthy targets", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			e.rpc(t, path, "sui_getLatestCheckpointSequenceNumber")
		}
	})
}
