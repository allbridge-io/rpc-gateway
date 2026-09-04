package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

const chainTypeSoroban = fakenode.ChainTypeSoroban

// newSorobanFixture starts a gateway with one soroban chain (SRB) served by two
// fake Soroban RPC nodes, loaded from a real TOML file like production does.
func newSorobanFixture(t *testing.T) (*fixture, *fakenode.Node, *fakenode.Node) {
	t.Helper()
	a := fakenode.New(t, "SdfA", chainTypeSoroban)
	b := fakenode.New(t, "SdfB", chainTypeSoroban)
	a.Set(fakenode.Behavior{Block: 4501689})
	b.Set(fakenode.Behavior{Block: 4501689})

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
[chains.SRB]
type = "soroban"
max_block_lag = 5
[[chains.SRB.targets]]
name = "SdfA"
http_url = "%s"
headers = { "X-Api-Key" = "test-key" }
[[chains.SRB.targets]]
name = "SdfB"
http_url = "%s"
`, a.URL(), b.URL())
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.LoadOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	rec := &events.Recorder{}
	gw, err := New(cfg, nil, rec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return &fixture{gw: gw, srv: srv, rec: rec}, a, b
}

func TestGateway_SorobanRouting(t *testing.T) {
	f, a, b := newSorobanFixture(t)

	for _, path := range []string{"/SRB", "/srb", "/Srb/"} {
		resp, body := f.do(t, http.MethodPost, path, `{"jsonrpc":"2.0","id":1,"method":"getLatestLedger"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		var parsed struct {
			Result struct {
				Sequence uint64 `json:"sequence"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Result.Sequence != 4501689 {
			t.Errorf("%s: unexpected body %s", path, body)
		}
		if p := resp.Header.Get("X-Rpc-Provider"); !strings.HasPrefix(p, "Sdf") {
			t.Errorf("%s: provider header %q", path, p)
		}
	}
	if a.CallCount("getLatestLedger")+b.CallCount("getLatestLedger") != 3 {
		t.Errorf("requests did not reach the SRB targets: a=%d b=%d",
			a.CallCount("getLatestLedger"), b.CallCount("getLatestLedger"))
	}
	// The API key of the target is added to proxied requests, not only to checks.
	for _, c := range a.Calls() {
		if c.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("api key header missing on a proxied request: %+v", c.Header)
		}
	}
}

// Soroban RPC is a single endpoint: a sub-path is a client mistake, not a
// route to be forwarded (that is what pass-through types such as tron do).
func TestGateway_SorobanRefusesSubPaths(t *testing.T) {
	f, a, b := newSorobanFixture(t)

	for _, path := range []string{"/SRB/getHealth", "/srb/soroban/rpc"} {
		resp, body := f.do(t, http.MethodPost, path, `{"jsonrpc":"2.0","id":1,"method":"getHealth"}`)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "single endpoint") {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
	if a.CallCount("")+b.CallCount("") != 0 {
		t.Error("a sub-path request must never reach a target")
	}
}

func TestGateway_SorobanMethodHandlingAndStatus(t *testing.T) {
	f, a, _ := newSorobanFixture(t)

	resp, _ := f.do(t, http.MethodGet, "/SRB", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("plain GET must be 405, got %d", resp.StatusCode)
	}
	resp, _ = f.do(t, http.MethodOptions, "/SRB", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS must be 204, got %d", resp.StatusCode)
	}

	a.Set(fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":{"status":"unhealthy","latestLedger":4501689}}`})
	f.gw.RunHealthChecksOnce(context.Background())

	resp, body := f.do(t, http.MethodGet, "/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, body)
	}
	srb := st.Chains["SRB"]
	if srb.Type != chainTypeSoroban || srb.RoutableTargets != 1 || len(srb.Targets) != 2 {
		t.Fatalf("SRB status: %+v", srb)
	}
	if srb.Targets[0].Routable || !strings.Contains(srb.Targets[0].LastError, `status "unhealthy"`) {
		t.Errorf("an RPC that reports itself unhealthy must be excluded: %+v", srb.Targets[0])
	}
	if !srb.Targets[1].Routable || srb.Targets[1].BlockNumber != 4501689 {
		t.Errorf("SdfB: %+v", srb.Targets[1])
	}
}
