package chaintype_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/chaintype"
	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

// config.ChainTypeSui is the config value of the Sui chain type.

func TestSui_RegisteredAsPassThrough(t *testing.T) {
	spec, ok := chaintype.Lookup(string(config.ChainTypeSui))
	if !ok {
		t.Fatalf("sui is not registered: %v", chaintype.Names())
	}
	if !spec.PassThrough {
		t.Error("sui serves GraphQL at /graphql under the base URL: the client's sub-path must be passed through")
	}
	if spec.Head == nil || spec.Doc == "" {
		t.Errorf("incomplete spec %+v", spec)
	}
	if !config.ChainTypeSui.PassThroughPath() {
		t.Error("config.ChainType(\"sui\").PassThroughPath() must be true")
	}
}

func TestSui_HeadQueriesTheLatestCheckpointOverGraphQL(t *testing.T) {
	n := fakenode.New(t, "Sui", config.ChainTypeSui)
	n.Set(fakenode.Behavior{Block: 336755149})

	block, err := head(t, config.ChainTypeSui, n, map[string]string{"X-Api-Key": "k"})
	if err != nil || block != 336755149 {
		t.Fatalf("head = %d, %v", block, err)
	}
	calls := n.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one health call, got %+v", calls)
	}
	c := calls[0]
	if c.Path != "/graphql" || c.HTTPMethod != http.MethodPost {
		t.Errorf("the health check must be POST /graphql, got %+v", c)
	}
	if !strings.Contains(c.Body, "checkpoint") || !strings.Contains(c.Body, "sequenceNumber") {
		t.Errorf("the health check must ask for checkpoint.sequenceNumber, sent %s", c.Body)
	}
	if c.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content type %q", c.Header.Get("Content-Type"))
	}
	if c.Header.Get("X-Api-Key") != "k" {
		t.Error("the target's headers must be sent with the health check")
	}
}

// QuickNode puts the token in the path (https://x.sui-mainnet.quiknode.pro/<token>/),
// with or without a trailing slash: /graphql goes under it either way.
func TestSui_HeadKeepsTheTargetBasePath(t *testing.T) {
	n := fakenode.New(t, "Sui", config.ChainTypeSui)
	n.Set(fakenode.Behavior{Block: 7})
	spec, _ := chaintype.Lookup(string(config.ChainTypeSui))

	for _, base := range []string{n.URL() + "/abc123", n.URL() + "/abc123/"} {
		n.ResetCalls()
		block, err := spec.Head(context.Background(), http.DefaultClient, base, nil)
		if err != nil || block != 7 {
			t.Fatalf("%s: head = %d, %v", base, block, err)
		}
		if got := n.Calls()[0].Path; got != "/abc123/graphql" {
			t.Errorf("%s: path = %q, want the base path kept", base, got)
		}
	}
}

// The checkpoint number is a JSON number in GraphQL. A decimal string, how Sui
// encodes a u64 elsewhere, is the same number and must not fail the check.
func TestSui_HeadAcceptsANumberOrADecimalString(t *testing.T) {
	for _, body := range []string{
		`{"data":{"checkpoint":{"sequenceNumber":336755149}}}`,
		`{"data":{"checkpoint":{"sequenceNumber":"336755149"}}}`,
	} {
		n := fakenode.New(t, "Sui", config.ChainTypeSui)
		n.Set(fakenode.Behavior{RawBody: body})

		block, err := head(t, config.ChainTypeSui, n, nil)
		if err != nil || block != 336755149 {
			t.Errorf("%s: head = %d, %v", body, block, err)
		}
	}
}

func TestSui_HeadFailureKinds(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 500", fakenode.Behavior{HTTPStatus: 500}, "graphql checkpoint: http status 500"},
		{"http 429", fakenode.Behavior{HTTPStatus: 429}, "http status 429"},
		// What a provider that dropped JSON-RPC but has no GraphQL at this base
		// URL answers (QuickNode's JSON-RPC root after the v1.82 upgrade).
		{"http 404 with an empty body", fakenode.Behavior{HTTPStatus: 404, RawBody: " "}, "http status 404: empty body"},
		{"not json", fakenode.Behavior{RawBody: "<html>bad gateway</html>"}, "invalid response"},
		{"graphql error", fakenode.Behavior{RawBody: `{"data":null,"errors":[{"message":"Unknown field \"checkpoint\"","extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`}, `graphql error: Unknown field "checkpoint"`},
		// A JSON-RPC server answering 200 at /graphql (publicnode's deprecation notice).
		{"json-rpc answer", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Sui JSON-RPC endpoint has been deprecated"},"id":null}`}, "no checkpoint.sequenceNumber"},
		{"null data", fakenode.Behavior{RawBody: `{"data":null}`}, "no checkpoint.sequenceNumber"},
		{"null checkpoint", fakenode.Behavior{RawBody: `{"data":{"checkpoint":null}}`}, "no checkpoint.sequenceNumber"},
		{"no sequenceNumber", fakenode.Behavior{RawBody: `{"data":{"checkpoint":{}}}`}, "no checkpoint.sequenceNumber"},
		{"not a number", fakenode.Behavior{RawBody: `{"data":{"checkpoint":{"sequenceNumber":"latest"}}}`}, "unexpected sequenceNumber"},
		{"hex instead of decimal", fakenode.Behavior{RawBody: `{"data":{"checkpoint":{"sequenceNumber":"0x10"}}}`}, "unexpected sequenceNumber"},
		{"negative", fakenode.Behavior{RawBody: `{"data":{"checkpoint":{"sequenceNumber":-1}}}`}, "unexpected sequenceNumber"},
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
