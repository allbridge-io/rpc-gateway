package chaintype_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/chaintype"
	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

func TestRegistry_Horizon(t *testing.T) {
	spec, ok := chaintype.Lookup(chaintype.Horizon)
	if !ok {
		t.Fatalf("horizon is not registered: %v", chaintype.Names())
	}
	if !spec.PassThrough {
		t.Error("horizon is a REST API: the client's sub-path must be passed through")
	}
	if spec.Name != chaintype.Horizon || spec.Head == nil || spec.Doc == "" {
		t.Errorf("incomplete spec %+v", spec)
	}
	if !config.ChainType(chaintype.Horizon).PassThroughPath() {
		t.Error("config must see horizon as a pass-through type")
	}
}

func TestHead_Horizon(t *testing.T) {
	n := fakenode.New(t, "H", fakenode.ChainTypeHorizon)
	n.Set(fakenode.Behavior{Block: 4501678})

	block, err := head(t, fakenode.ChainTypeHorizon, n, map[string]string{"X-Api-Key": "k"})
	if err != nil || block != 4501678 {
		t.Fatalf("head = %d, %v", block, err)
	}
	call := n.Calls()[0]
	if call.HTTPMethod != http.MethodGet || call.Path != "/" {
		t.Errorf("the health check must be GET / on the base URL, got %+v", call)
	}
	if call.Header.Get("X-Api-Key") != "k" {
		t.Error("the target's headers must be sent with the health check")
	}
}

// Horizon's own error document is served with a real status code, so the
// generic non-200 rule already rejects it; the other kinds are the ways a
// reverse proxy or a half-synced instance can answer 200 with something useless.
func TestHead_HorizonFailureKinds(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 503", fakenode.Behavior{HTTPStatus: 503}, "http status 503"},
		{"http 429", fakenode.Behavior{HTTPStatus: 429}, "http status 429"},
		{
			"problem+json 404",
			fakenode.HorizonProblem(404, "not_found", "Resource Missing", "The resource at the url requested was not found."),
			"http status 404",
		},
		{"not json", fakenode.Behavior{RawBody: "<html>Bad Gateway</html>"}, "horizon root: invalid response"},
		{"json but not an object", fakenode.Behavior{RawBody: `["a"]`}, "horizon root: invalid response"},
		{"no history_latest_ledger", fakenode.Behavior{RawBody: `{"core_latest_ledger":42}`}, "no history_latest_ledger"},
		{"nothing ingested yet", fakenode.Behavior{RawBody: `{"history_latest_ledger":0}`}, "history_latest_ledger is 0"},
		{"dropped connection", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "H", fakenode.ChainTypeHorizon)
			n.Set(tt.b)

			block, err := head(t, fakenode.ChainTypeHorizon, n, nil)
			if err == nil {
				t.Fatalf("expected an error, got ledger %d", block)
			}
			if !containsAny(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// A base path in http_url (a provider prefix) must survive the health check.
func TestHead_HorizonKeepsTheTargetBasePath(t *testing.T) {
	n := fakenode.New(t, "H", fakenode.ChainTypeHorizon)
	n.Set(fakenode.Behavior{Block: 7})
	spec, _ := chaintype.Lookup(chaintype.Horizon)

	// The fake node only serves the root document at "/", so this call fails;
	// what is asserted is where it was sent. Horizon's root document *is* the
	// health call, so the target URL must be used as-is, with nothing appended.
	_, _ = spec.Head(context.Background(), http.DefaultClient, n.URL()+"/horizon", nil)
	if got := n.Calls()[0].Path; got != "/horizon" {
		t.Errorf("path = %q, want the target's base path used as-is", got)
	}
}

// The example config must keep loading with the Stellar Horizon chain in it.
// (internal/config/config_test.go owns the generic example-config test; this
// one covers only the key this chain type adds.)
func TestHorizon_ExampleConfigHasTheChain(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{
		ConfigPath: filepath.Join("..", "..", "config.example.toml"),
		LookupEnv:  func(string) (string, bool) { return "example", true },
	})
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	chain, ok := cfg.Chains["SRB_HORIZON"]
	if !ok {
		t.Fatalf("example config lacks chain SRB_HORIZON: %v", cfg.ChainKeys())
	}
	if chain.Type != config.ChainType(chaintype.Horizon) {
		t.Errorf("SRB_HORIZON type = %q", chain.Type)
	}
	if len(chain.Targets) == 0 || chain.Targets[0].HTTPURL == "" {
		t.Errorf("SRB_HORIZON has no usable target: %+v", chain.Targets)
	}
	if got := cfg.MaxBlockLagFor("SRB_HORIZON"); got != 5 {
		t.Errorf("max_block_lag = %d, want 5 (ledgers close every ~5s)", got)
	}
}
