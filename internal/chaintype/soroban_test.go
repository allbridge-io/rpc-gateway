package chaintype_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/chaintype"
	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

const chainTypeSoroban = fakenode.ChainTypeSoroban

func TestRegistry_SorobanIsASingleEndpointType(t *testing.T) {
	spec, ok := chaintype.Lookup(string(chainTypeSoroban))
	if !ok {
		t.Fatalf("soroban is not registered: %v", chaintype.Names())
	}
	if spec.PassThrough {
		// Soroban RPC is one JSON-RPC endpoint; a sub-path must stay a 404.
		t.Error("soroban must not pass the client's sub-path through")
	}
	if spec.Head == nil || spec.Doc == "" {
		t.Errorf("incomplete spec %+v", spec)
	}
}

func TestHead_Soroban(t *testing.T) {
	n := fakenode.New(t, "SRB", chainTypeSoroban)
	n.Set(fakenode.Behavior{Block: 4501689})

	block, err := head(t, chainTypeSoroban, n, map[string]string{"X-Api-Key": "k"})
	if err != nil || block != 4501689 {
		t.Fatalf("head = %d, %v", block, err)
	}
	call := n.Calls()[0]
	if call.Method != "getHealth" || call.HTTPMethod != http.MethodPost {
		t.Errorf("unexpected health call %+v", call)
	}
	if call.Header.Get("X-Api-Key") != "k" {
		t.Error("the target's headers must be sent with the health check")
	}
	// Soroban RPC rejects a "params" array; the check must omit the member.
	if strings.Contains(call.Body, `"params"`) {
		t.Errorf("the health call must send no params member: %s", call.Body)
	}
}

// The fake node answers a params array the way the real server does. This test
// exists so that switching the check to the shared callJSONRPC (which always
// sends "params": []) can never pass unnoticed.
func TestHead_SorobanRejectsAParamsArrayLikeTheRealServer(t *testing.T) {
	n := fakenode.New(t, "SRB", chainTypeSoroban)
	n.Set(fakenode.Behavior{Block: 10})
	spec, _ := chaintype.Lookup(string(chainTypeSoroban))

	body := `{"jsonrpc":"2.0","id":1,"method":"getHealth","params":[]}`
	resp, err := http.Post(n.URL(), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	read, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:read]), "invalid parameters") {
		t.Fatalf("the fake node must reject a params array: %s", buf[:read])
	}

	if block, err := spec.Head(t.Context(), http.DefaultClient, n.URL(), nil); err != nil || block != 10 {
		t.Fatalf("the registered check must still succeed: %d, %v", block, err)
	}
}

func TestHead_SorobanFailureKinds(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 500", fakenode.Behavior{HTTPStatus: 500}, "http status 500"},
		{"http 429", fakenode.Behavior{HTTPStatus: 429}, "http status 429"},
		{"not json", fakenode.Behavior{RawBody: "<html>bad gateway</html>"}, "invalid json-rpc response"},
		{"json-rpc error", fakenode.Behavior{RPCError: "method not found"}, "method not found"},
		{"null result", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":null}`}, "empty json-rpc result"},
		{"result is not an object", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":"healthy"}`}, "getHealth: unexpected result"},
		{"no status", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":{"latestLedger":7}}`}, "no status in response"},
		{"status not healthy", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":{"status":"unhealthy","latestLedger":7}}`}, `status "unhealthy"`},
		{"no latestLedger", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":{"status":"healthy"}}`}, "no latestLedger"},
		{"dropped connection", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "SRB", chainTypeSoroban)
			n.Set(tt.b)

			block, err := head(t, chainTypeSoroban, n, nil)
			if err == nil {
				t.Fatalf("expected an error, got ledger %d", block)
			}
			if !containsAny(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// The example config must keep a working soroban chain: internal/config's own
// test only names the chains it knew when it was written.
func TestSoroban_ExampleConfigHasTheChain(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{
		ConfigPath: filepath.Join("..", "..", "config.example.toml"),
		LookupEnv:  func(string) (string, bool) { return "example", true },
	})
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	chain, ok := cfg.Chains["SRB"]
	if !ok {
		t.Fatal("config.example.toml lacks chain SRB")
	}
	if chain.Type != chainTypeSoroban {
		t.Errorf("SRB type = %q, want soroban", chain.Type)
	}
	if len(chain.Targets) == 0 {
		t.Error("SRB has no targets")
	}
	if lag := cfg.MaxBlockLagFor("SRB"); lag == 0 || lag > 20 {
		t.Errorf("SRB max_block_lag = %d; ledgers close every ~5s, keep it tight", lag)
	}
}
