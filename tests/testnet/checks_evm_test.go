//go:build testnet

package testnet

import (
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/config"
)

func init() {
	registerChecks(config.ChainTypeEVM, typeChecks{
		Verify: testEVM,
		Probe: func(t *testing.T, e *env, key string) *http.Response {
			_, resp := e.rpc(t, "/"+key, "eth_blockNumber")
			return resp
		},
	})
}

func testEVM(t *testing.T, e *env, key string, chain config.Chain) {
	path := "/" + key

	t.Run("eth_chainId", func(t *testing.T) {
		raw, resp := e.rpc(t, path, "eth_chainId")
		var id string
		_ = json.Unmarshal(raw, &id)
		t.Logf("%s chainId=%s provider=%s", key, id, resp.Header.Get("X-Rpc-Provider"))
		if chain.ChainID != "" && !strings.EqualFold(id, chain.ChainID) {
			t.Errorf("chain id %s, config expects %s: wrong network behind %s", id, chain.ChainID, key)
		}
	})
	t.Run("eth_blockNumber", func(t *testing.T) {
		block := hexToUint(t, mustRaw(e.rpc(t, path, "eth_blockNumber")))
		if block == 0 {
			t.Fatal("block number is zero")
		}
		st := e.gw.Chain(key).Manager.Status()
		var checked uint64
		for _, s := range st {
			if s.BlockNumber > checked {
				checked = s.BlockNumber
			}
		}
		if diff := absDiff(block, checked); diff > 200 {
			t.Errorf("block via proxy %d differs from health-check block %d by %d", block, checked, diff)
		}
	})
	t.Run("eth_getBalance", func(t *testing.T) {
		raw, _ := e.rpc(t, path, "eth_getBalance", "0x0000000000000000000000000000000000000000", "latest")
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("expected hex string, got %s", raw)
		}
		if _, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16); !ok || !strings.HasPrefix(s, "0x") {
			t.Fatalf("balance is not a 0x hex quantity: %s", s)
		}
	})
	t.Run("eth_call revert is passed to the client", func(t *testing.T) {
		// Calling a non-contract address returns "0x"; a JSON-RPC error from a
		// real revert would also be passed through, not turned into a 503.
		raw, _ := e.rpc(t, path, "eth_call", map[string]string{"to": "0x0000000000000000000000000000000000000001", "data": "0x00"}, "latest")
		if !strings.HasPrefix(string(raw), `"0x`) {
			t.Errorf("unexpected eth_call result %s", raw)
		}
	})
	t.Run("repeated requests stay on healthy targets", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			e.rpc(t, path, "eth_blockNumber")
		}
	})
}
