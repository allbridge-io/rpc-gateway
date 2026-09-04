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

// chainTypeStacks is the chains.X.type value. This package deliberately does
// not import internal/testutil, which holds the constant for the offline tests.
const chainTypeStacks config.ChainType = "stacks"

func init() {
	registerChecks(chainTypeStacks, typeChecks{
		Verify: func(t *testing.T, e *env, key string, _ config.Chain) { testStacks(t, e, key) },
		Probe: func(t *testing.T, e *env, key string) *http.Response {
			resp, data := e.do(t, http.MethodGet, "/"+key+"/v2/info", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
			}
			return resp
		},
	})
}

// stacksInfo is the part of /v2/info the checks read.
type stacksInfo struct {
	StacksTipHeight uint64 `json:"stacks_tip_height"`
	BurnBlockHeight uint64 `json:"burn_block_height"`
	NetworkID       uint64 `json:"network_id"`
	ServerVersion   string `json:"server_version"`
}

func testStacks(t *testing.T, e *env, key string) {
	path := "/" + key

	t.Run("v2 info", func(t *testing.T) {
		resp, data := e.do(t, http.MethodGet, path+"/v2/info", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		if provider := resp.Header.Get("X-Rpc-Provider"); provider == "" || provider == deadTargetName {
			t.Fatalf("served by %q", provider)
		}
		var info stacksInfo
		if err := json.Unmarshal(data, &info); err != nil {
			t.Fatalf("not JSON: %s", truncate(data))
		}
		t.Logf("%s tip=%d burn=%d network_id=%d version=%q provider=%s",
			key, info.StacksTipHeight, info.BurnBlockHeight, info.NetworkID, info.ServerVersion, resp.Header.Get("X-Rpc-Provider"))
		if info.StacksTipHeight == 0 {
			t.Fatalf("stacks_tip_height is zero in %s", truncate(data))
		}
		if info.NetworkID == 0 {
			t.Errorf("no network_id in %s", truncate(data))
		}

		// The health check reads the very same field, so the two must agree
		// within the few blocks produced since the last check round.
		var checked uint64
		for _, s := range e.gw.Chain(key).Manager.Status() {
			if s.BlockNumber > checked {
				checked = s.BlockNumber
			}
		}
		if diff := absDiff(info.StacksTipHeight, checked); diff > 100 {
			t.Errorf("tip via proxy %d differs from health-check block %d by %d", info.StacksTipHeight, checked, diff)
		}
	})

	t.Run("extended v1 block", func(t *testing.T) {
		resp, data := e.do(t, http.MethodGet, path+"/extended/v1/block?limit=1", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		var blocks struct {
			Results []struct {
				Height    uint64 `json:"height"`
				Hash      string `json:"hash"`
				Canonical bool   `json:"canonical"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &blocks); err != nil {
			t.Fatalf("not JSON: %s", truncate(data))
		}
		if len(blocks.Results) != 1 {
			t.Fatalf("limit=1 must return exactly one block: %s", truncate(data))
		}
		if blocks.Results[0].Height == 0 || !strings.HasPrefix(blocks.Results[0].Hash, "0x") {
			t.Errorf("unexpected block %+v", blocks.Results[0])
		}
	})

	t.Run("client error passed through", func(t *testing.T) {
		// A malformed principal: Hiro answers 400 with its own JSON body. That is
		// the client's mistake, so it must arrive untouched and cause no reroute.
		before := stacksReroutesOf(e, key)
		resp, data := e.do(t, http.MethodGet, path+"/extended/v1/address/SPINVALID/balances", nil)
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 400/404 from Hiro, got HTTP %d %s", resp.StatusCode, truncate(data))
		}
		if !strings.Contains(string(data), `"message"`) && !strings.Contains(string(data), `"error"`) {
			t.Errorf("expected Hiro's JSON error body, got %s", truncate(data))
		}
		if after := stacksReroutesOf(e, key); after > before {
			t.Errorf("client error must not cause a reroute: %d new reroutes", after-before)
		}
	})

	t.Run("repeated requests stay on healthy targets", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			resp, data := e.do(t, http.MethodGet, path+"/v2/info", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("request %d: HTTP %d %s", i, resp.StatusCode, truncate(data))
			}
			if provider := resp.Header.Get("X-Rpc-Provider"); provider == deadTargetName {
				t.Fatalf("request %d answered by the dead target", i)
			}
		}
	})
}

// stacksReroutesOf counts the reroutes recorded for one chain so far.
func stacksReroutesOf(e *env, key string) int {
	count := 0
	for _, ev := range e.rec.Of(events.KindRerouted, "") {
		if ev.Chain == key {
			count++
		}
	}
	return count
}
