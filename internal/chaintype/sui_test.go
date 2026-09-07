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

// config.ChainTypeSui is the config value of the Sui chain type.

func TestSui_Registered(t *testing.T) {
	spec, ok := chaintype.Lookup(string(config.ChainTypeSui))
	if !ok {
		t.Fatalf("sui is not registered: %v", chaintype.Names())
	}
	if spec.PassThrough {
		t.Error("sui is a single JSON-RPC endpoint; the client's sub-path must not be passed through")
	}
	if spec.Head == nil || spec.Doc == "" {
		t.Errorf("incomplete spec %+v", spec)
	}
	if config.ChainTypeSui.PassThroughPath() {
		t.Error("config.ChainType(\"sui\").PassThroughPath() must be false")
	}
}

func TestSui_Head(t *testing.T) {
	n := fakenode.New(t, "Sui", config.ChainTypeSui)
	n.Set(fakenode.Behavior{Block: 379726054})

	block, err := head(t, config.ChainTypeSui, n, map[string]string{"X-Api-Key": "k"})
	if err != nil || block != 379726054 {
		t.Fatalf("head = %d, %v", block, err)
	}
	calls := n.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one health call, got %+v", calls)
	}
	if calls[0].Method != "sui_getLatestCheckpointSequenceNumber" || calls[0].HTTPMethod != http.MethodPost {
		t.Errorf("unexpected health call %+v", calls[0])
	}
	if calls[0].Path != "/" {
		t.Errorf("a single-endpoint type must use the target URL as-is, got path %q", calls[0].Path)
	}
	if calls[0].Header.Get("X-Api-Key") != "k" {
		t.Error("the target's headers must be sent with the health check")
	}
}

// Sui encodes u64 as a decimal string; the checkpoint number is already past
// what a float64 would represent exactly, so the string form must be parsed
// rather than run through a JSON number.
func TestSui_HeadParsesLargeDecimalStrings(t *testing.T) {
	n := fakenode.New(t, "Sui", config.ChainTypeSui)
	n.Set(fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":"9007199254740993"}`})

	block, err := head(t, config.ChainTypeSui, n, nil)
	if err != nil || block != 9007199254740993 {
		t.Fatalf("head = %d, %v; want 9007199254740993", block, err)
	}
}

// A provider answering with a bare JSON number instead of the documented string
// is still understood: the number is the same, and failing the check would take
// a working target out of rotation for a formatting detail.
func TestSui_HeadAcceptsABareNumber(t *testing.T) {
	n := fakenode.New(t, "Sui", config.ChainTypeSui)
	n.Set(fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":42}`})

	block, err := head(t, config.ChainTypeSui, n, nil)
	if err != nil || block != 42 {
		t.Fatalf("head = %d, %v; want 42", block, err)
	}
}

func TestSui_HeadFailureKinds(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 500", fakenode.Behavior{HTTPStatus: 500}, "http status 500"},
		{"http 429", fakenode.Behavior{HTTPStatus: 429}, "http status 429"},
		{"not json", fakenode.Behavior{RawBody: "<html>bad gateway</html>"}, "invalid json-rpc response"},
		{"json-rpc error", fakenode.Behavior{RPCError: "Method not found"}, "Method not found"},
		{"missing result", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1}`}, "empty json-rpc result"},
		{"null result", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":null}`}, "empty json-rpc result"},
		{"not a number", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":"latest"}`}, "unexpected result"},
		{"hex instead of decimal", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":"0x10"}`}, "unexpected result"},
		{"object instead of a number", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":{"seq":7}}`}, "unexpected result"},
		{"dropped connection", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "Sui", config.ChainTypeSui)
			n.Set(tt.b)

			block, err := head(t, config.ChainTypeSui, n, nil)
			if err == nil {
				t.Fatalf("expected an error, got block %d", block)
			}
			if !containsAny(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestSui_HeadHonoursTheContext(t *testing.T) {
	n := fakenode.New(t, "Sui", config.ChainTypeSui)
	n.Set(fakenode.Behavior{Hang: true})
	spec, _ := chaintype.Lookup(string(config.ChainTypeSui))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := spec.Head(ctx, http.DefaultClient, n.URL(), nil); err == nil {
		t.Fatal("a cancelled context must fail the check instead of hanging")
	}
}

// The shipped example config must keep describing a working SUI chain;
// internal/config only asserts the chains that predate the registry.
func TestSui_ExampleConfigHasTheChain(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{
		ConfigPath: filepath.Join("..", "..", "config.example.toml"),
		LookupEnv:  func(string) (string, bool) { return "example", true },
	})
	if err != nil {
		t.Fatalf("load config.example.toml: %v", err)
	}
	chain, ok := cfg.Chains["SUI"]
	if !ok {
		t.Fatalf("example config lacks chain SUI: %v", cfg.ChainKeys())
	}
	if chain.Type != config.ChainTypeSui {
		t.Errorf("SUI type = %q, want sui", chain.Type)
	}
	if len(chain.Targets) == 0 {
		t.Error("SUI has no targets")
	}
	if chain.MaxBlockLag == nil || *chain.MaxBlockLag == 0 {
		t.Error("SUI must set max_block_lag: checkpoints advance every ~250ms, the global default of 20 is far too tight")
	}
}
