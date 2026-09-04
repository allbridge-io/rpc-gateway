//go:build testnet

package testnet

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/0xProject/rpc-gateway/internal/config"
)

func init() {
	registerChecks(config.ChainTypeSolana, typeChecks{
		Verify: func(t *testing.T, e *env, key string, _ config.Chain) { testSolana(t, e, key) },
		Probe: func(t *testing.T, e *env, key string) *http.Response {
			_, resp := e.rpc(t, "/"+key, "getSlot")
			return resp
		},
	})
}

func testSolana(t *testing.T, e *env, key string) {
	path := "/" + key

	t.Run("getHealth", func(t *testing.T) {
		raw, resp := e.rpc(t, path, "getHealth")
		t.Logf("%s getHealth=%s provider=%s", key, raw, resp.Header.Get("X-Rpc-Provider"))
		if string(raw) != `"ok"` {
			t.Errorf("getHealth = %s", raw)
		}
	})
	t.Run("getSlot", func(t *testing.T) {
		var slot uint64
		if err := json.Unmarshal(mustRaw(e.rpc(t, path, "getSlot")), &slot); err != nil || slot == 0 {
			t.Fatalf("getSlot: %v", err)
		}
	})
	t.Run("getLatestBlockhash", func(t *testing.T) {
		raw, _ := e.rpc(t, path, "getLatestBlockhash", map[string]string{"commitment": "finalized"})
		if !strings.Contains(string(raw), `"blockhash"`) {
			t.Errorf("no blockhash in %s", raw)
		}
	})
	t.Run("websocket slotSubscribe", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), wsTimeout)
		defer cancel()
		wsURL := strings.Replace(e.base, "http://", "ws://", 1) + path
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("ws dial %s: %v", wsURL, err)
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":1,"method":"slotSubscribe"}`)); err != nil {
			t.Fatalf("ws write: %v", err)
		}
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("ws read (no slotNotification within %v): %v", wsTimeout, err)
			}
			if strings.Contains(string(data), `"slotNotification"`) {
				t.Logf("received %s", truncate(data))
				return
			}
			if strings.Contains(string(data), `"error"`) {
				t.Fatalf("subscription error: %s", data)
			}
		}
	})
}
