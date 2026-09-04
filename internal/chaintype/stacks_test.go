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

func TestStacks_RegisteredAsPassThrough(t *testing.T) {
	spec, ok := chaintype.Lookup(string(fakenode.ChainTypeStacks))
	if !ok {
		t.Fatalf("stacks is not registered: %v", chaintype.Names())
	}
	if !spec.PassThrough {
		t.Error("stacks serves the whole Hiro REST API: the client's sub-path must be passed through")
	}
	if spec.Doc == "" || spec.Head == nil {
		t.Errorf("incomplete spec %+v", spec)
	}
	if !fakenode.ChainTypeStacks.PassThroughPath() {
		t.Error("config must see stacks as a pass-through type")
	}
}

func TestStacks_HeadReadsStacksTipHeight(t *testing.T) {
	n := fakenode.New(t, "Hiro", fakenode.ChainTypeStacks)
	n.Set(fakenode.Behavior{Block: 256844})

	block, err := head(t, fakenode.ChainTypeStacks, n, map[string]string{"X-API-Key": "k"})
	if err != nil || block != 256844 {
		t.Fatalf("head = %d, %v", block, err)
	}
	call := n.Calls()[0]
	if call.Path != "/v2/info" || call.HTTPMethod != http.MethodGet {
		t.Errorf("the health check must be GET /v2/info, got %+v", call)
	}
	if call.Header.Get("X-API-Key") != "k" {
		t.Error("the target's headers must be sent with the health check")
	}
}

// A base path in http_url (a provider prefix or an API key path) must survive
// the health check.
func TestStacks_HeadKeepsTheTargetBasePath(t *testing.T) {
	n := fakenode.New(t, "Hiro", fakenode.ChainTypeStacks)
	n.Set(fakenode.Behavior{Block: 7})
	spec, _ := chaintype.Lookup(string(fakenode.ChainTypeStacks))

	block, err := spec.Head(context.Background(), http.DefaultClient, n.URL()+"/stacks", nil)
	if err != nil || block != 7 {
		t.Fatalf("head = %d, %v", block, err)
	}
	if got := n.Calls()[0].Path; got != "/stacks/v2/info" {
		t.Errorf("path = %q, want the base path kept", got)
	}
}

func TestStacks_HeadFailureKinds(t *testing.T) {
	// The Hiro API reports failures with a real status code and a JSON body of
	// its own shape; a CDN in front of it can answer with anything at all.
	const hiroError = `{"statusCode":429,"error":"Too Many Requests","message":"rate limit exceeded"}`
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 500", fakenode.Behavior{HTTPStatus: 500}, "v2/info: http status 500"},
		{"http 429 with a hiro body", fakenode.Behavior{HTTPStatus: 429, RawBody: hiroError}, "http status 429: {\"statusCode\":429"},
		{"http 404 (wrong base url)", fakenode.Behavior{HTTPStatus: 404}, "http status 404"},
		{"not json", fakenode.Behavior{RawBody: "<html>gateway timeout</html>"}, "v2/info: invalid response"},
		{"no stacks_tip_height", fakenode.Behavior{RawBody: `{"network_id":2147483648}`}, "no stacks_tip_height"},
		{"height of the wrong type", fakenode.Behavior{RawBody: `{"stacks_tip_height":"256844"}`}, "invalid response"},
		{"dropped connection", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "Hiro", fakenode.ChainTypeStacks)
			n.Set(tt.b)

			block, err := head(t, fakenode.ChainTypeStacks, n, nil)
			if err == nil {
				t.Fatalf("expected an error, got block %d", block)
			}
			if !containsAny(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// A node that honestly reports height 0 is not a malformed answer: only a
// missing field is. The block-lag check is what excludes a target that far
// behind the others.
func TestStacks_HeadAcceptsHeightZero(t *testing.T) {
	n := fakenode.New(t, "Fresh", fakenode.ChainTypeStacks)
	n.Set(fakenode.Behavior{Block: 0})

	block, err := head(t, fakenode.ChainTypeStacks, n, nil)
	if err != nil || block != 0 {
		t.Fatalf("head = %d, %v", block, err)
	}
}

// The example config must keep loading with the STX chain in it;
// internal/config/config_test.go only knows the chains that predate it.
func TestStacks_ExampleConfigHasSTX(t *testing.T) {
	cfg, err := config.Load(config.LoadOptions{
		ConfigPath: filepath.Join("..", "..", "config.example.toml"),
		LookupEnv:  func(string) (string, bool) { return "example", true },
	})
	if err != nil {
		t.Fatalf("load config.example.toml: %v", err)
	}
	chain, ok := cfg.Chains["STX"]
	if !ok {
		t.Fatalf("example config lacks chain STX: %v", cfg.ChainKeys())
	}
	if chain.Type != fakenode.ChainTypeStacks {
		t.Errorf("STX type = %q, want stacks", chain.Type)
	}
	if len(chain.Targets) == 0 {
		t.Error("STX has no targets")
	}
	if chain.MaxBlockLag == nil || *chain.MaxBlockLag == 0 {
		t.Error("STX should keep a block lag limit: Stacks blocks are seconds apart")
	}
}
