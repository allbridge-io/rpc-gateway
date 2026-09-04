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

// tonFixture is a gateway serving a single TON chain backed by one fake node.
type tonFixture struct {
	gw   *Gateway
	srv  *httptest.Server
	node *fakenode.Node
}

func newTonFixture(t *testing.T) *tonFixture {
	t.Helper()
	node := fakenode.New(t, "Toncenter", fakenode.TypeTON)
	node.Set(fakenode.Behavior{Block: 82614017})

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
[chains.TON]
type = "ton"
max_block_lag = 5
[[chains.TON.targets]]
name = "Toncenter"
http_url = "%s"
headers = { "X-API-Key" = "test-key" }
`, node.URL())
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
	return &tonFixture{gw: gw, srv: srv, node: node}
}

func (f *tonFixture) do(t *testing.T, method, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
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

func TestGateway_TonPassThroughRoutes(t *testing.T) {
	f := newTonFixture(t)

	resp, body := f.do(t, http.MethodGet, "/TON/api/v3/masterchainInfo", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"seqno":82614017`) {
		t.Errorf("GET /TON/api/v3/masterchainInfo: %d %s", resp.StatusCode, body)
	}
	// The chain key is case-insensitive and the query must survive.
	resp, body = f.do(t, http.MethodGet, "/ton/api/v3/blocks?limit=1&sort=desc", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with sub-path and query: %d %s", resp.StatusCode, body)
	}
	var echo struct {
		HTTPMethod string `json:"httpMethod"`
		Path       string `json:"path"`
		Query      string `json:"query"`
	}
	if err := json.Unmarshal([]byte(body), &echo); err != nil {
		t.Fatalf("not a fakenode echo: %s", body)
	}
	if echo.HTTPMethod != http.MethodGet || echo.Path != "/api/v3/blocks" || echo.Query != "limit=1&sort=desc" {
		t.Errorf("sub-path and query not passed through: %+v", echo)
	}
	resp, body = f.do(t, http.MethodPost, "/TON/api/v2/jsonRPC", `{"jsonrpc":"2.0","id":1,"method":"getMasterchainInfo","params":{}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"ok":true`) {
		t.Errorf("POST /TON/api/v2/jsonRPC: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(t, http.MethodPost, "/TON", "{}")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"path":"/"`) {
		t.Errorf("bare /TON must reach the target root: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.do(t, http.MethodOptions, "/TON/api/v3/blocks", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS: %d", resp.StatusCode)
	}
	for _, c := range f.node.Calls() {
		if c.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("api key header missing on %s", c.Path)
		}
	}
}

func TestGateway_TonStatusReportsTheSeqno(t *testing.T) {
	f := newTonFixture(t)
	f.gw.RunHealthChecksOnce(context.Background())

	resp, body := f.do(t, http.MethodGet, "/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(body, `"type":"ton"`) || !strings.Contains(body, `"blockNumber":82614017`) {
		t.Errorf("status must report the ton chain and its head: %s", body)
	}
}

func TestGateway_TonUnknownChainStillUnknown(t *testing.T) {
	f := newTonFixture(t)

	// A pass-through chain must not swallow other prefixes.
	resp, body := f.do(t, http.MethodGet, "/TONX/api/v3/blocks", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/TONX: %d %s", resp.StatusCode, body)
	}
}
