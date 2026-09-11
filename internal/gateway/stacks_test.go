package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

type stacksFixture struct {
	gw  *Gateway
	srv *httptest.Server
	stx *fakenode.Node
	spl *fakenode.Node
}

// newStacksFixture loads a real TOML config with one Stacks chain (STX) and one
// EVM chain, so the routing of a pass-through type can be compared with the
// single-endpoint one in the same gateway.
func newStacksFixture(t *testing.T) *stacksFixture {
	t.Helper()
	stx := fakenode.New(t, "Hiro", config.ChainTypeStacks)
	stx.Set(fakenode.Behavior{Block: 256844})
	f := newStacksFixtureFor(t, stx.URL(), "")
	f.stx = stx
	return f
}

// newStacksFixtureFor is newStacksFixture with any HTTP server as the STX
// target (it must serve /v2/info) and extra lines for the [server] table.
func newStacksFixtureFor(t *testing.T, stxURL, serverTOML string) *stacksFixture {
	t.Helper()
	spl := fakenode.New(t, "Spl", config.ChainTypeEVM)
	spl.Set(fakenode.Behavior{Block: 100})

	toml := fmt.Sprintf(`
[server]
port = 1
upstream_timeout = "1s"
%s
[healthchecks]
interval = "1h"
timeout = "1s"
failure_threshold = 1
success_threshold = 1
taint_duration = "0s"

[chains.STX]
type = "stacks"
max_block_lag = 5
[[chains.STX.targets]]
name = "Hiro"
http_url = "%s"
headers = { "x-api-key" = "test-key" }

[chains.SPL]
type = "evm"
[[chains.SPL.targets]]
name = "Spl"
http_url = "%s"
`, serverTOML, stxURL, spl.URL())
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.LoadOptions{ConfigPath: path})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	gw, err := New(cfg, nil, &events.Recorder{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return &stacksFixture{gw: gw, srv: srv, spl: spl}
}

func (f *stacksFixture) call(t *testing.T, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestGateway_StacksPassThroughRoutes(t *testing.T) {
	f := newStacksFixture(t)

	// The Hiro REST API: any method, any sub-path, query kept.
	resp, body := f.call(t, http.MethodGet, "/STX/extended/v1/block?limit=1", "")
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"path":"/extended/v1/block"`) || !strings.Contains(body, `"query":"limit=1"`) {
		t.Errorf("GET with sub-path and query: %d %s", resp.StatusCode, body)
	}
	// The node endpoints Hiro proxies, reached through the same prefix.
	resp, body = f.call(t, http.MethodGet, "/stx/v2/info", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"stacks_tip_height":256844`) {
		t.Errorf("lowercase key and /v2/info: %d %s", resp.StatusCode, body)
	}
	resp, body = f.call(t, http.MethodPost, "/STX/v2/transactions", `{"tx":"80800000"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"httpMethod":"POST"`) {
		t.Errorf("POST /v2/transactions: %d %s", resp.StatusCode, body)
	}
	resp, body = f.call(t, http.MethodGet, "/STX", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"path":"/"`) {
		t.Errorf("bare /STX must reach the target root: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.call(t, http.MethodOptions, "/STX/extended/v1/block", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS: %d", resp.StatusCode)
	}
	for _, c := range f.stx.Calls() {
		if c.Header.Get("x-api-key") != "test-key" {
			t.Errorf("api key header missing on %s", c.Path)
		}
	}
	// A single-endpoint chain in the same gateway still refuses sub-paths.
	resp, _ = f.call(t, http.MethodPost, "/SPL/v2/info", "{}")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("sub-path on an evm chain must be 404, got %d", resp.StatusCode)
	}
	resp, body = f.call(t, http.MethodGet, "/BTC/v2/info", "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "SPL, STX") {
		t.Errorf("unknown chain: %d %s", resp.StatusCode, body)
	}
}

func TestGateway_StacksStatusReportsTipHeight(t *testing.T) {
	f := newStacksFixture(t)
	f.gw.RunHealthChecksOnce(context.Background())

	resp, body := f.call(t, http.MethodGet, "/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var st Status
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, body)
	}
	stx := st.Chains["STX"]
	if stx.Type != config.ChainTypeStacks || stx.RoutableTargets != 1 {
		t.Fatalf("STX status: %+v", stx)
	}
	if !stx.Targets[0].Routable || stx.Targets[0].BlockNumber != 256844 {
		t.Errorf("STX target: %+v", stx.Targets[0])
	}
	if c := f.stx.Calls()[0]; c.Path != "/v2/info" || c.HTTPMethod != http.MethodGet {
		t.Errorf("the health check must be GET /v2/info, got %+v", c)
	}
}

func TestGateway_StacksNoRoutableTargetIs503(t *testing.T) {
	f := newStacksFixture(t)
	f.stx.Set(fakenode.Behavior{HTTPStatus: 500})
	f.gw.RunHealthChecksOnce(context.Background())

	resp, body := f.call(t, http.MethodGet, "/STX/extended/v1/block", "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "chain STX") {
		t.Errorf("STX must be 503 when its only target is down: %d %s", resp.StatusCode, body)
	}
	// The EVM chain of the same gateway keeps working.
	resp, _ = f.call(t, http.MethodPost, "/SPL", rpcBody)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("SPL must keep working: %d", resp.StatusCode)
	}
}

// Hiro answers GET / with "301 Location: /extended". Passed through as is, that
// sends a redirect-following client to /extended on the gateway, where the API
// key and the chain are missing from the path and the answer is a 401 the
// client did not cause. The gateway moves the Location back under the prefix
// the client used, key and chain spelled the way the client spelled them.
func TestGateway_StacksUpstreamRedirectStaysBehindTheKey(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"stacks_tip_height":256844}`)
	})
	mux.HandleFunc("/extended", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "extended api") })
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://docs.hiro.so/", http.StatusFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/extended?from=root", http.StatusMovedPermanently)
	})
	hiro := httptest.NewServer(mux)
	t.Cleanup(hiro.Close)
	f := newStacksFixtureFor(t, hiro.URL, `api_keys = ["`+key+`"]`)

	for _, path := range []string{"/" + key + "/STX", "/" + key + "/stx/"} {
		resp, _ := f.call(t, http.MethodGet, path, "")
		want := path[:len("/"+key+"/STX")] + "/extended?from=root"
		if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != want {
			t.Errorf("GET %s: %d Location %q, want 301 %q", path, resp.StatusCode, resp.Header.Get("Location"), want)
		}
	}
	// Following the rewritten redirect lands on the API, not on a 401.
	resp, body := f.call(t, http.MethodGet, "/"+key+"/STX/extended?from=root", "")
	if resp.StatusCode != http.StatusOK || body != "extended api" {
		t.Errorf("following the rewritten Location: %d %s", resp.StatusCode, body)
	}
	// A redirect away from the target is not the gateway's to rewrite.
	resp, _ = f.call(t, http.MethodGet, "/"+key+"/STX/elsewhere", "")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "https://docs.hiro.so/" {
		t.Errorf("foreign redirect: %d Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}
