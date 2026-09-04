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

func init() {
	registerChecks(config.ChainTypeTron, typeChecks{
		Verify: testTron,
		Probe: func(t *testing.T, e *env, key string) *http.Response {
			resp, data := e.do(t, http.MethodPost, "/"+key+"/wallet/getnowblock", []byte("{}"))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
			}
			return resp
		},
	})
}

func testTron(t *testing.T, e *env, key string, chain config.Chain) {
	path := "/" + key

	t.Run("wallet getnowblock", func(t *testing.T) {
		resp, data := e.do(t, http.MethodPost, path+"/wallet/getnowblock", []byte("{}"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		var b struct {
			BlockHeader struct {
				RawData struct {
					Number uint64 `json:"number"`
				} `json:"raw_data"`
			} `json:"block_header"`
		}
		if err := json.Unmarshal(data, &b); err != nil || b.BlockHeader.RawData.Number == 0 {
			t.Fatalf("no block number in %s", truncate(data))
		}
		t.Logf("%s block=%d provider=%s", key, b.BlockHeader.RawData.Number, resp.Header.Get("X-Rpc-Provider"))
	})
	t.Run("wallet getaccount via GET with query", func(t *testing.T) {
		resp, data := e.do(t, http.MethodGet, path+"/wallet/getaccount?address=TJRabPrwbZy45sbavfcjinPJC18kjpRTv8&visible=true", nil)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"address"`) {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
	})
	t.Run("client error passed through", func(t *testing.T) {
		resp, data := e.do(t, http.MethodPost, path+"/wallet/getaccount", []byte(`{"address":"not-an-address","visible":true}`))
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"Error"`) {
			t.Fatalf("expected Tron's 200 + Error body, got HTTP %d %s", resp.StatusCode, truncate(data))
		}
		if ev := e.rec.Of(events.KindRerouted, ""); len(ev) != 0 {
			t.Errorf("client error must not cause a reroute: %+v", ev)
		}
	})
	t.Run("jsonrpc eth_chainId", func(t *testing.T) {
		raw, _ := e.rpc(t, path+"/jsonrpc", "eth_chainId")
		var id string
		_ = json.Unmarshal(raw, &id)
		if chain.ChainID != "" && !strings.EqualFold(id, chain.ChainID) {
			t.Errorf("chain id %s, config expects %s", id, chain.ChainID)
		}
	})
	t.Run("v1 events (TronGrid only)", func(t *testing.T) {
		resp, data := e.do(t, http.MethodGet, path+"/v1/blocks/latest/events?limit=1", nil)
		if resp.StatusCode == http.StatusNotFound {
			t.Skip("target is not TronGrid; /v1 API not available")
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"data"`) {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
	})
}
