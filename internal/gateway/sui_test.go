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

// suiQuery is a GraphQL query the fake node does not simulate, so the answer
// is an echo of what reached it.
const suiQuery = `{"query":"{ object(address: \"0x2\") { version } }"}`

// suiEcho is what fakenode answers for a request it does not simulate.
type suiEcho struct {
	Node       string `json:"node"`
	HTTPMethod string `json:"httpMethod"`
	Path       string `json:"path"`
	Query      string `json:"query"`
	Body       string `json:"body"`
}

func decodeSuiEcho(t *testing.T, body string) suiEcho {
	t.Helper()
	var e suiEcho
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("not a fakenode echo: %s", body)
	}
	return e
}

// newSuiFixture adds a SUI chain to the shared fixture and returns its node.
// The target URL carries a token path, the way QuickNode hands it out.
func newSuiFixture(t *testing.T) (*fixture, *fakenode.Node) {
	t.Helper()
	n := fakenode.New(t, "Sui", config.ChainTypeSui)
	n.Set(fakenode.Behavior{Block: 336755149})
	f := newFixture(t, fmt.Sprintf(`
[chains.SUI]
type = "sui"
max_block_lag = 100
[[chains.SUI.targets]]
name = "Sui"
http_url = "%s/token/"
headers = { "X-Api-Key" = "test-key" }
`, n.URL()))
	return f, n
}

func TestGatewaySui_GraphQLPassesThrough(t *testing.T) {
	f, n := newSuiFixture(t)

	for _, path := range []string{"/SUI/graphql", "/sui/graphql", "/Sui/graphql?x=1"} {
		resp, body := f.do(t, http.MethodPost, path, suiQuery, "Content-Type", "application/json")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		e := decodeSuiEcho(t, body)
		if e.Node != "Sui" || e.Path != "/token/graphql" || e.HTTPMethod != http.MethodPost || e.Body != suiQuery {
			t.Errorf("%s: not forwarded to the target's /graphql: %+v", path, e)
		}
		if resp.Header.Get("X-Rpc-Provider") != "Sui" {
			t.Errorf("%s: provider header %q", path, resp.Header.Get("X-Rpc-Provider"))
		}
	}
	for _, c := range n.Calls() {
		if c.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("api key header missing on %s", c.Path)
		}
	}
}

// A JSON-RPC client posting to the bare chain URL still reaches the target's
// root, so a provider that keeps serving JSON-RPC there keeps working.
func TestGatewaySui_RootReachesTheTargetBaseURL(t *testing.T) {
	f, _ := newSuiFixture(t)

	const rpc = `{"jsonrpc":"2.0","id":1,"method":"suix_getAllBalances","params":["0x2"]}`
	for _, path := range []string{"/SUI", "/SUI/"} {
		resp, body := f.do(t, http.MethodPost, path, rpc)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
		if e := decodeSuiEcho(t, body); e.Path != "/token/" || e.Body != rpc {
			t.Errorf("%s: %+v", path, e)
		}
	}
	resp, _ := f.do(t, http.MethodOptions, "/SUI/graphql", "")
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
	if sui.Targets[0].BlockNumber != 336755149 {
		t.Errorf("checkpoint sequence number not reported: %+v", sui.Targets[0])
	}
	if c := n.Calls()[0]; c.Path != "/token/graphql" || c.HTTPMethod != http.MethodPost {
		t.Errorf("the health check must be POST <base>/graphql, got %+v", c)
	}

	// A dead SUI chain must not take the other chains down with it.
	n.Set(fakenode.Behavior{HTTPStatus: 500})
	f.gw.RunHealthChecksOnce(context.Background())
	resp, body := f.do(t, http.MethodPost, "/SUI/graphql", suiQuery)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "chain SUI") {
		t.Errorf("SUI must be 503 once its only target fails: %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, http.MethodPost, "/SPL", rpcBody); resp.StatusCode != http.StatusOK {
		t.Errorf("SPL must keep working, got %d", resp.StatusCode)
	}
}
