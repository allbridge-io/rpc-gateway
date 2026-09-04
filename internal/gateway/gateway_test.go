package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

const rpcBody = `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`

type fixture struct {
	gw   *Gateway
	srv  *httptest.Server
	rec  *events.Recorder
	spl1 *fakenode.Node
	spl2 *fakenode.Node
	sol  *fakenode.Node
	trx  *fakenode.Node
}

// newFixture loads a real TOML config (the same path production uses) with
// two EVM targets for SPL and one Solana target for SOL.
func newFixture(t *testing.T, extraTOML string) *fixture {
	t.Helper()
	spl1 := fakenode.New(t, "Spl1", config.ChainTypeEVM)
	spl2 := fakenode.New(t, "Spl2", config.ChainTypeEVM)
	sol := fakenode.New(t, "Sol", config.ChainTypeSolana)
	trx := fakenode.New(t, "Trx", config.ChainTypeTron)
	spl1.Set(fakenode.Behavior{Block: 100})
	spl2.Set(fakenode.Behavior{Block: 100})
	sol.Set(fakenode.Behavior{Block: 5000})
	trx.Set(fakenode.Behavior{Block: 777})

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
%s
[chains.SPL]
type = "evm"
chain_id = "0xaa36a7"
[[chains.SPL.targets]]
name = "Spl1"
http_url = "%s"
[[chains.SPL.targets]]
name = "Spl2"
http_url = "%s"
[chains.SOL]
type = "solana"
max_block_lag = 0
[[chains.SOL.targets]]
name = "Sol"
http_url = "%s"
[chains.TRX]
type = "tron"
[[chains.TRX.targets]]
name = "Trx"
http_url = "%s"
headers = { "TRON-PRO-API-KEY" = "test-key" }
`, extraTOML, spl1.URL(), spl2.URL(), sol.URL(), trx.URL())
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
	return &fixture{gw: gw, srv: srv, rec: rec, spl1: spl1, spl2: spl2, sol: sol, trx: trx}
}

func (f *fixture) do(t *testing.T, method, path, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
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

func nodeOf(t *testing.T, body string) string {
	t.Helper()
	var resp struct {
		Result struct {
			Node string `json:"node"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("not a fakenode response: %s", body)
	}
	return resp.Result.Node
}

func TestGateway_RoutesByChainKeyCaseInsensitive(t *testing.T) {
	f := newFixture(t, "")

	for _, path := range []string{"/SPL", "/spl", "/Spl/", "/SPL?x=1"} {
		resp, body := f.do(t, http.MethodPost, path, rpcBody)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(nodeOf(t, body), "Spl") {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
		if p := resp.Header.Get("X-Rpc-Provider"); !strings.HasPrefix(p, "Spl") {
			t.Errorf("%s: provider header %q", path, p)
		}
	}
	resp, body := f.do(t, http.MethodPost, "/sol", rpcBody)
	if resp.StatusCode != http.StatusOK || nodeOf(t, body) != "Sol" {
		t.Errorf("/sol: %d %s", resp.StatusCode, body)
	}
	if f.sol.CallCount("eth_call") != 1 || f.spl1.CallCount("eth_call")+f.spl2.CallCount("eth_call") != 4 {
		t.Errorf("requests reached the wrong chain: sol=%d spl=%d", f.sol.CallCount(""), f.spl1.CallCount("")+f.spl2.CallCount(""))
	}
}

func TestGateway_UnknownRoutes404(t *testing.T) {
	f := newFixture(t, "")
	for _, path := range []string{"/", "/BTC", "/SPL/extra", "/status/x"} {
		resp, body := f.do(t, http.MethodPost, path, rpcBody)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
		if !strings.Contains(body, `"error"`) {
			t.Errorf("%s: expected a JSON-RPC style error, got %s", path, body)
		}
	}
	_, body := f.do(t, http.MethodPost, "/BTC", rpcBody)
	if !strings.Contains(body, "SOL, SPL, TRX") {
		t.Errorf("unknown chain error should list configured chains: %s", body)
	}
}

func TestGateway_TronPassThroughRoutes(t *testing.T) {
	f := newFixture(t, "")

	resp, body := f.do(t, http.MethodPost, "/TRX/wallet/getnowblock", "{}")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"number":777`) {
		t.Errorf("POST /TRX/wallet/getnowblock: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(t, http.MethodGet, "/trx/v1/accounts/TAbc/transactions?limit=2", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"path":"/v1/accounts/TAbc/transactions"`) || !strings.Contains(body, `"query":"limit=2"`) {
		t.Errorf("GET with sub-path and query: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(t, http.MethodPost, "/TRX/jsonrpc", `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"result":"0x1"`) {
		t.Errorf("POST /TRX/jsonrpc: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(t, http.MethodPost, "/TRX", "{}")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"path":"/"`) {
		t.Errorf("bare /TRX must reach the target root: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.do(t, http.MethodOptions, "/TRX/wallet/getnowblock", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS: %d", resp.StatusCode)
	}
	for _, c := range f.trx.Calls() {
		if c.Header.Get("TRON-PRO-API-KEY") != "test-key" {
			t.Errorf("api key header missing on %s", c.Path)
		}
	}
	// Single-endpoint chains still refuse sub-paths.
	resp, _ = f.do(t, http.MethodPost, "/SPL/wallet/getnowblock", "{}")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("sub-path on an evm chain must be 404, got %d", resp.StatusCode)
	}
}

func TestGateway_MethodHandling(t *testing.T) {
	f := newFixture(t, "")

	resp, _ := f.do(t, http.MethodGet, "/SPL", "")
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") == "" {
		t.Errorf("plain GET must be 405 with Allow, got %d", resp.StatusCode)
	}
	resp, _ = f.do(t, http.MethodOptions, "/SPL", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS must be 204, got %d", resp.StatusCode)
	}
	resp, _ = f.do(t, http.MethodDelete, "/SPL", "")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE must be 405, got %d", resp.StatusCode)
	}
}

func TestGateway_HealthzIsLivenessOnly(t *testing.T) {
	f := newFixture(t, "")
	f.spl1.Set(fakenode.Behavior{HTTPStatus: 500})
	f.spl2.Set(fakenode.Behavior{HTTPStatus: 500})
	f.sol.Set(fakenode.Behavior{Drop: true})
	f.gw.RunHealthChecksOnce(context.Background())

	resp, body := f.do(t, http.MethodGet, "/healthz", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"healthy":true`) {
		t.Errorf("healthz must stay 200 even when every upstream is down: %d %s", resp.StatusCode, body)
	}
}

func TestGateway_StatusReflectsHealth(t *testing.T) {
	f := newFixture(t, "")
	f.spl2.Set(fakenode.Behavior{HTTPStatus: 500})
	f.gw.RunHealthChecksOnce(context.Background())

	resp, body := f.do(t, http.MethodGet, "/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, body)
	}
	spl := st.Chains["SPL"]
	if spl.Type != config.ChainTypeEVM || spl.RoutableTargets != 1 || len(spl.Targets) != 2 {
		t.Errorf("SPL status: %+v", spl)
	}
	if spl.Targets[0].Name != "Spl1" || !spl.Targets[0].Routable || spl.Targets[0].BlockNumber != 100 {
		t.Errorf("Spl1: %+v", spl.Targets[0])
	}
	if spl.Targets[1].Routable || !strings.Contains(spl.Targets[1].LastError, "500") {
		t.Errorf("Spl2 must be unhealthy with the error: %+v", spl.Targets[1])
	}
	sol := st.Chains["SOL"]
	if sol.Type != config.ChainTypeSolana || sol.RoutableTargets != 1 || sol.Targets[0].BlockNumber != 5000 {
		t.Errorf("SOL status: %+v", sol)
	}
	if strings.Contains(body, f.spl1.URL()) {
		t.Error("status must not leak target URLs (they may contain API keys)")
	}
}

func TestGateway_ChainsAreIsolated(t *testing.T) {
	f := newFixture(t, "")
	f.sol.Set(fakenode.Behavior{HTTPStatus: 500})
	f.gw.RunHealthChecksOnce(context.Background())

	resp, body := f.do(t, http.MethodPost, "/SOL", rpcBody)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "chain SOL") {
		t.Errorf("SOL must be 503: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(t, http.MethodPost, "/SPL", rpcBody)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(nodeOf(t, body), "Spl") {
		t.Errorf("SPL must keep working: %d %s", resp.StatusCode, body)
	}
	nh := f.rec.Of(events.KindNoHealthy, "")
	if len(nh) != 1 || nh[0].Chain != "SOL" {
		t.Errorf("expected exactly one no-healthy event for SOL, got %+v", nh)
	}
}

func TestGateway_ExceptionsGlobalAndPerChain(t *testing.T) {
	f := newFixture(t, `
[[exceptions]]
match = "global-oops"
[[chains.SOL.exceptions]]
match = "sol-only"
`)
	// Global exception applies to SPL: both nodes answer with it -> 503.
	f.spl1.Set(fakenode.Behavior{RPCError: "global-oops"})
	f.spl2.Set(fakenode.Behavior{RPCError: "global-oops"})
	resp, _ := f.do(t, http.MethodPost, "/SPL", rpcBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("global exception must apply to SPL, got %d", resp.StatusCode)
	}
	// Chain-specific exception does not apply to SPL: passed through as a normal error.
	f.spl1.Set(fakenode.Behavior{RPCError: "sol-only"})
	f.spl2.Set(fakenode.Behavior{RPCError: "sol-only"})
	resp, body := f.do(t, http.MethodPost, "/SPL", rpcBody)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "sol-only") {
		t.Errorf("SOL-only exception must not affect SPL: %d %s", resp.StatusCode, body)
	}
	// ...but it does apply to SOL.
	f.sol.Set(fakenode.Behavior{RPCError: "sol-only"})
	resp, _ = f.do(t, http.MethodPost, "/SOL", rpcBody)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("chain exception must apply to SOL, got %d", resp.StatusCode)
	}
}

func TestGateway_ListenAndServeGracefulShutdown(t *testing.T) {
	f := newFixture(t, "")
	f.gw.cfg.Server.Port = 0 // let the OS pick a free port
	f.gw.cfg.Server.ShutdownTimeout = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.gw.ListenAndServe(ctx) }()

	addrCtx, addrCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer addrCancel()
	addr, err := f.gw.Addr(addrCtx)
	if err != nil {
		t.Fatalf("server did not start: %v", err)
	}
	resp, err := http.Post("http://"+addr+"/SPL", "application/json", strings.NewReader(rpcBody))
	if err != nil {
		t.Fatalf("request to live server: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
	if f.spl1.CallCount("eth_blockNumber")+f.spl2.CallCount("eth_blockNumber") < 2 {
		t.Error("initial health round must run before serving")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ListenAndServe returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete")
	}
}

func TestGateway_PortInUseIsAnError(t *testing.T) {
	f := newFixture(t, "")
	// Occupy a port on the same wildcard address the gateway binds to.
	blocker, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	f.gw.cfg.Server.Port = uint(blocker.Addr().(*net.TCPAddr).Port)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = f.gw.ListenAndServe(ctx)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("expected a listen error, got %v", err)
	}
}
