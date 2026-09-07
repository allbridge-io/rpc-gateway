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

func TestTON_Registered(t *testing.T) {
	spec, ok := chaintype.Lookup("ton")
	if !ok {
		t.Fatalf("ton is not registered: %v", chaintype.Names())
	}
	if !spec.PassThrough {
		t.Error("ton serves the whole toncenter REST API: the client's path must be passed through")
	}
	if spec.Name != "ton" || spec.Head == nil || spec.Doc == "" {
		t.Errorf("incomplete spec %+v", spec)
	}
}

func TestHead_TON(t *testing.T) {
	n := fakenode.New(t, "Toncenter", config.ChainTypeTON)
	n.Set(fakenode.Behavior{Block: 82614017})

	block, err := head(t, config.ChainTypeTON, n, map[string]string{"X-API-Key": "secret"})
	if err != nil || block != 82614017 {
		t.Fatalf("head = %d, %v", block, err)
	}
	call := n.Calls()[0]
	if call.Path != "/api/v3/masterchainInfo" || call.HTTPMethod != http.MethodGet {
		t.Errorf("unexpected health call %+v", call)
	}
	if call.Header.Get("X-API-Key") != "secret" {
		t.Error("the target's headers must be sent with the health check (toncenter API key)")
	}
}

// A base path in http_url (a provider prefix or an API key path) must survive
// the health check.
func TestHead_TONKeepsTheTargetBasePath(t *testing.T) {
	n := fakenode.New(t, "Toncenter", config.ChainTypeTON)
	n.Set(fakenode.Behavior{Block: 7})
	spec, _ := chaintype.Lookup(string(config.ChainTypeTON))

	if _, err := spec.Head(context.Background(), http.DefaultClient, n.URL()+"/base", nil); err != nil {
		t.Fatalf("head: %v", err)
	}
	if got := n.Calls()[0].Path; got != "/base/api/v3/masterchainInfo" {
		t.Errorf("path = %q, want the base path kept", got)
	}
}

func TestHead_TONFailureKinds(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 500", fakenode.Behavior{HTTPStatus: 500}, "http status 500"},
		{"rate limited", fakenode.Behavior{HTTPStatus: 429, RawBody: `{"error":"Rate limit exceeded"}`}, "http status 429"},
		{
			"toncenter error body with a 4xx",
			fakenode.Behavior{HTTPStatus: 422, RawBody: `{"error":"failed to decode"}`},
			"failed to decode",
		},
		{"toncenter error body with a 200", fakenode.Behavior{RawBody: `{"error":"lite server timeout"}`}, "masterchainInfo: lite server timeout"},
		{"not json", fakenode.Behavior{RawBody: "<html>"}, "invalid response"},
		{"no last.seqno", fakenode.Behavior{RawBody: `{"first":{"seqno":1}}`}, "no last.seqno"},
		{"seqno zero", fakenode.Behavior{Block: 0}, "no last.seqno"},
		{"dropped connection", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "Toncenter", config.ChainTypeTON)
			n.Set(tt.b)

			block, err := head(t, config.ChainTypeTON, n, nil)
			if err == nil {
				t.Fatalf("expected an error, got block %d", block)
			}
			if !containsAny(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// The example config in the repo root must ship a working TON chain: it is the
// template users copy, and internal/config only proves that the file loads.
func TestTON_InExampleConfig(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{
		ConfigPath: filepath.Join("..", "..", "config.example.toml"),
		LookupEnv: func(name string) (string, bool) {
			return "example", name == "ALCHEMY_KEY"
		},
	})
	if err != nil {
		t.Fatalf("load config.example.toml: %v", err)
	}
	chain, ok := cfg.Chains["TON"]
	if !ok {
		t.Fatal("the example config lacks chain TON")
	}
	if chain.Type != config.ChainTypeTON {
		t.Errorf("TON type = %q, want ton", chain.Type)
	}
	if !chain.Type.PassThroughPath() {
		t.Error("TON must pass the client's path through")
	}
	if len(chain.Targets) == 0 {
		t.Fatal("TON has no targets")
	}
}
