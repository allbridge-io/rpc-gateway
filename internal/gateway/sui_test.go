package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

// config.ChainTypeSui is the config value of the Sui chain type.

const suiRPCBody = `{"jsonrpc":"2.0","id":1,"method":"suix_getAllBalances","params":["0x2"]}`

// newSuiFixture adds a SUI chain to the shared fixture and returns its node.
func newSuiFixture(t *testing.T) (*fixture, *fakenode.Node) {
	t.Helper()
	n := fakenode.New(t, "Sui", config.ChainTypeSui)
	n.Set(fakenode.Behavior{Block: 379726054})
	f := newFixture(t, fmt.Sprintf(`
[chains.SUI]
type = "sui"
max_block_lag = 100
[[chains.SUI.targets]]
name = "Sui"
http_url = "%s"
headers = { "X-Api-Key" = "test-key" }
`, n.URL()))
	return f, n
}

func TestGatewaySui_RoutesCaseInsensitively(t *testing.T) {
	f, n := newSuiFixture(t)

	for _, path := range []string{"/SUI", "/sui", "/Sui/", "/SUI?x=1"} {
		resp, body := f.do(t, http.MethodPost, path, suiRPCBody)
		if resp.StatusCode != http.StatusOK || nodeOf(t, body) != "Sui" {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
		if resp.Header.Get("X-Rpc-Provider") != "Sui" {
			t.Errorf("%s: provider header %q", path, resp.Header.Get("X-Rpc-Provider"))
		}
	}
	if n.CallCount("suix_getAllBalances") != 4 {
		t.Errorf("the SUI chain received %d calls, want 4: %+v", n.CallCount("suix_getAllBalances"), n.Calls())
	}
	for _, c := range n.Calls() {
		if c.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("api key header missing on %s", c.Path)
		}
	}
}

// Sui serves a single JSON-RPC endpoint, so a sub-path is a client mistake and
// must not be forwarded (that would silently drop it and answer the root).
func TestGatewaySui_SubPathIs404(t *testing.T) {
	f, n := newSuiFixture(t)

	for _, path := range []string{"/SUI/v1", "/sui/sui_getObject"} {
		resp, body := f.do(t, http.MethodPost, path, suiRPCBody)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
		if !strings.Contains(body, "single endpoint") {
			t.Errorf("%s: unhelpful error %s", path, body)
		}
	}
	if n.CallCount("suix_getAllBalances") != 0 {
		t.Errorf("a sub-path request must never reach the target: %+v", n.Calls())
	}
}

func TestGatewaySui_MethodHandling(t *testing.T) {
	f, _ := newSuiFixture(t)

	resp, _ := f.do(t, http.MethodGet, "/SUI", "")
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") == "" {
		t.Errorf("plain GET must be 405 with Allow, got %d", resp.StatusCode)
	}
	resp, _ = f.do(t, http.MethodOptions, "/SUI", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS must be 204, got %d", resp.StatusCode)
	}
}

func TestGatewaySui_StatusReportsTheCheckpoint(t *testing.T) {
	f, n := newSuiFixture(t)
	f.gw.RunHealthChecksOnce(context.Background())

	_, body := f.do(t, http.MethodGet, "/status", "")
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, body)
	}
	sui, ok := st.Chains["SUI"]
	if !ok {
		t.Fatalf("SUI missing from status: %s", body)
	}
	if sui.Type != config.ChainTypeSui || sui.RoutableTargets != 1 || len(sui.Targets) != 1 {
		t.Fatalf("SUI status: %+v", sui)
	}
	if sui.Targets[0].BlockNumber != 379726054 {
		t.Errorf("checkpoint sequence number not reported: %+v", sui.Targets[0])
	}
	if n.CallCount("sui_getLatestCheckpointSequenceNumber") == 0 {
		t.Error("the health check never ran against the SUI target")
	}

	// A dead SUI chain must not take the other chains down with it.
	n.Set(fakenode.Behavior{HTTPStatus: 500})
	f.gw.RunHealthChecksOnce(context.Background())
	resp, body := f.do(t, http.MethodPost, "/SUI", suiRPCBody)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "chain SUI") {
		t.Errorf("SUI must be 503 once its only target fails: %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, http.MethodPost, "/SPL", rpcBody); resp.StatusCode != http.StatusOK {
		t.Errorf("SPL must keep working, got %d", resp.StatusCode)
	}
}
