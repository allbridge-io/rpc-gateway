// Package chaintype_test is an external test package on purpose: the fake node
// it drives imports internal/config, which imports internal/chaintype.
// Every internal/chaintype/<type>_test.go must be in this package too.
package chaintype_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/chaintype"
	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

// head runs the registered health check of a type against a fake node.
func head(t *testing.T, typ config.ChainType, n *fakenode.Node, headers map[string]string) (uint64, error) {
	t.Helper()
	spec, ok := chaintype.Lookup(string(typ))
	if !ok {
		t.Fatalf("chain type %q is not registered", typ)
	}
	return spec.Head(context.Background(), http.DefaultClient, n.URL(), headers)
}

func TestRegistry_KnownTypes(t *testing.T) {
	names := chaintype.Names()
	for _, want := range []string{"evm", "solana", "tron"} {
		if !containsString(names, want) {
			t.Errorf("%q is missing from the registry: %v", want, names)
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("Names() must be sorted and unique: %v", names)
		}
	}
	if _, ok := chaintype.Lookup("cosmos"); ok {
		t.Error("an unregistered type must not be found")
	}
}

func TestRegistry_PassThrough(t *testing.T) {
	tests := map[string]bool{"evm": false, "solana": false, "tron": true}
	for name, want := range tests {
		spec, ok := chaintype.Lookup(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if spec.PassThrough != want {
			t.Errorf("%s: PassThrough = %v, want %v", name, spec.PassThrough, want)
		}
		if spec.Name != name || spec.Head == nil || spec.Doc == "" {
			t.Errorf("%s: incomplete spec %+v", name, spec)
		}
	}
}

func TestRegistry_DuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("registering a name twice must panic")
		}
	}()
	chaintype.Register(chaintype.Spec{Name: "evm", Head: func(context.Context, *http.Client, string, map[string]string) (uint64, error) {
		return 0, nil
	}})
}

func TestHead_EVM(t *testing.T) {
	n := fakenode.New(t, "E", config.ChainTypeEVM)
	n.Set(fakenode.Behavior{Block: 0x1234})

	block, err := head(t, config.ChainTypeEVM, n, map[string]string{"X-Api-Key": "k"})
	if err != nil || block != 0x1234 {
		t.Fatalf("head = %d, %v", block, err)
	}
	call := n.Calls()[0]
	if call.Method != "eth_blockNumber" || call.HTTPMethod != http.MethodPost {
		t.Errorf("unexpected health call %+v", call)
	}
	if call.Header.Get("X-Api-Key") != "k" {
		t.Error("the target's headers must be sent with the health check")
	}
}

func TestHead_Solana(t *testing.T) {
	n := fakenode.New(t, "S", config.ChainTypeSolana)
	n.Set(fakenode.Behavior{Block: 4242})

	block, err := head(t, config.ChainTypeSolana, n, nil)
	if err != nil || block != 4242 {
		t.Fatalf("head = %d, %v", block, err)
	}
	if n.CallCount("getSlot") != 1 {
		t.Errorf("solana must be checked with getSlot: %+v", n.Calls())
	}
}

func TestHead_Tron(t *testing.T) {
	n := fakenode.New(t, "T", config.ChainTypeTron)
	n.Set(fakenode.Behavior{Block: 68000000})

	block, err := head(t, config.ChainTypeTron, n, map[string]string{"TRON-PRO-API-KEY": "k"})
	if err != nil || block != 68000000 {
		t.Fatalf("head = %d, %v", block, err)
	}
	call := n.Calls()[0]
	if call.Path != "/wallet/getnowblock" || call.HTTPMethod != http.MethodPost {
		t.Errorf("unexpected health call %+v", call)
	}
	if call.Header.Get("TRON-PRO-API-KEY") != "k" {
		t.Error("the target's headers must be sent with the health check")
	}
}

// A base path in http_url (an API key path, a gateway prefix) must survive the
// health check of a pass-through type.
func TestHead_TronKeepsTheTargetBasePath(t *testing.T) {
	n := fakenode.New(t, "T", config.ChainTypeTron)
	n.Set(fakenode.Behavior{Block: 7})
	spec, _ := chaintype.Lookup(string(config.ChainTypeTron))

	if _, err := spec.Head(context.Background(), http.DefaultClient, n.URL()+"/base", nil); err != nil {
		t.Fatalf("head: %v", err)
	}
	if got := n.Calls()[0].Path; got != "/base/wallet/getnowblock" {
		t.Errorf("path = %q, want the base path kept", got)
	}
}

func TestHead_FailureKinds(t *testing.T) {
	tests := []struct {
		name    string
		typ     config.ChainType
		b       fakenode.Behavior
		wantErr string
	}{
		{"evm http 500", config.ChainTypeEVM, fakenode.Behavior{HTTPStatus: 500}, "http status 500"},
		{"evm invalid json", config.ChainTypeEVM, fakenode.Behavior{RawBody: "<html>"}, "invalid json-rpc response"},
		{"evm json-rpc error", config.ChainTypeEVM, fakenode.Behavior{RPCError: "boom"}, "boom"},
		{"evm not hex", config.ChainTypeEVM, fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":"0xzz"}`}, "invalid hex"},
		{"solana wrong type", config.ChainTypeSolana, fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":"x"}`}, "getSlot: unexpected result"},
		{"tron error body", config.ChainTypeTron, fakenode.Behavior{TronError: "OutOfMemoryError"}, "getnowblock: OutOfMemoryError"},
		{"tron no block number", config.ChainTypeTron, fakenode.Behavior{RawBody: `{"blockID":"x"}`}, "no block number"},
		{"tron dropped connection", config.ChainTypeTron, fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "N", tt.typ)
			n.Set(tt.b)

			block, err := head(t, tt.typ, n, nil)
			if err == nil {
				t.Fatalf("expected an error, got block %d", block)
			}
			if !containsAny(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// containsAny reports whether s contains at least one of the '|'-separated alternatives.
func containsAny(s, alternatives string) bool {
	for _, alt := range strings.Split(alternatives, "|") {
		if strings.Contains(s, alt) {
			return true
		}
	}
	return false
}
