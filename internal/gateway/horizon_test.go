package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

// horizonFixture is a gateway serving one Stellar Horizon chain next to an EVM
// one, so the routing of a pass-through chain and of a single-endpoint chain
// can be compared in the same instance.
type horizonFixture struct {
	srv *httptest.Server
	hor *fakenode.Node
	evm *fakenode.Node
}

func newHorizonFixture(t *testing.T) *horizonFixture {
	t.Helper()
	hor := fakenode.New(t, "SDF", fakenode.ChainTypeHorizon)
	hor.Set(fakenode.Behavior{Block: 4501678})
	evm := fakenode.New(t, "Evm", config.ChainTypeEVM)
	evm.Set(fakenode.Behavior{Block: 100})

	toml := fmt.Sprintf(`
[server]
port = 1
upstream_timeout = "1s"
[healthchecks]
interval = "1h"
timeout = "1s"
failure_threshold = 1
success_threshold = 1
taint_duration = "0s"

[chains.SRB_HORIZON]
type = "horizon"
max_block_lag = 5
[[chains.SRB_HORIZON.targets]]
name = "SDF"
http_url = "%s"
headers = { "X-Api-Key" = "test-key" }

[chains.SPL]
type = "evm"
[[chains.SPL.targets]]
name = "Evm"
http_url = "%s"
`, hor.URL(), evm.URL())

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.LoadOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	gw, err := New(cfg, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return &horizonFixture{srv: srv, hor: hor, evm: evm}
}

func (f *horizonFixture) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(f.srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, sb.String()
}

func TestGateway_HorizonPassThroughRoutes(t *testing.T) {
	f := newHorizonFixture(t)

	// The bare chain key reaches Horizon's root document, which is also its
	// health check: a client can read the ingested ledger through the gateway.
	resp, body := f.get(t, "/SRB_HORIZON")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"history_latest_ledger":4501678`) {
		t.Errorf("GET /SRB_HORIZON: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Rpc-Provider") != "SDF" {
		t.Errorf("provider header %q", resp.Header.Get("X-Rpc-Provider"))
	}

	resp, body = f.get(t, "/srb_horizon/ledgers?order=desc&limit=1")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"sequence":4501678`) {
		t.Errorf("lower-case key with a sub-path: %d %s", resp.StatusCode, body)
	}

	resp, body = f.get(t, "/SRB_HORIZON/accounts/GABC/payments?limit=2&order=asc")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("deep sub-path: %d %s", resp.StatusCode, body)
	}
	var echo struct {
		Path  string `json:"path"`
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(body), &echo); err != nil {
		t.Fatalf("not a fakenode echo: %s", body)
	}
	if echo.Path != "/accounts/GABC/payments" || echo.Query != "limit=2&order=asc" {
		t.Errorf("deep sub-path and query not passed through: %+v", echo)
	}

	for _, c := range f.hor.Calls() {
		if c.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("api key header missing on %s", c.Path)
		}
	}

	// A single-endpoint chain in the same gateway still refuses sub-paths.
	resp, _ = f.get(t, "/SPL/ledgers")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("sub-path on an evm chain must be 404, got %d", resp.StatusCode)
	}
}

func TestGateway_HorizonPassesEveryMethodAndKeepsErrors(t *testing.T) {
	f := newHorizonFixture(t)

	// GET is a normal request for a REST chain, not a WebSocket upgrade attempt
	// and not a 405 as it would be on a JSON-RPC chain.
	resp, _ := f.get(t, "/SPL")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET on an evm chain must stay 405, got %d", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodDelete, f.srv.URL+"/SRB_HORIZON/offers/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("any HTTP method must be forwarded by a pass-through chain, got %d", resp.StatusCode)
	}

	// Horizon's own 404 for an unknown resource reaches the client unchanged.
	f.hor.Set(fakenode.HorizonProblem(404, "not_found", "Resource Missing", "The resource at the url requested was not found."))
	resp, body := f.get(t, "/SRB_HORIZON/accounts/GNOSUCHACCOUNT")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "not_found") {
		t.Errorf("Horizon's 404 must be passed through: %d %s", resp.StatusCode, body)
	}
}
