package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

// config.ChainTypeSui is the config value of the Sui chain type.

// suiEchoBody is a request the fake node does not simulate, so the answer names
// the node that served it.
const suiEchoBody = `{"jsonrpc":"2.0","id":7,"method":"suix_getAllBalances","params":["0x2"]}`

func newSuiProxy(t *testing.T, targets []config.Target, exceptions []config.Exception, taint time.Duration) *testProxy {
	t.Helper()
	rec := &events.Recorder{}
	clock := newFakeClock()
	opts := defaultOpts()
	opts.TaintDuration = taint
	opts.Now = clock.Now
	m := NewManager("SUI", config.ChainTypeSui, targets, opts, rec)
	px, err := New(Options{
		Chain:           "SUI",
		Type:            config.ChainTypeSui,
		Targets:         targets,
		Exceptions:      exceptions,
		UpstreamTimeout: 2 * time.Second,
		Observer:        rec,
	}, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testProxy{Proxy: px, rec: rec, clock: clock}
}

func TestSui_HealthCheckUsesTheLatestCheckpoint(t *testing.T) {
	n := fakenode.New(t, "PublicNode", config.ChainTypeSui)
	n.Set(fakenode.Behavior{Block: 379726054})
	target := n.Target()
	target.Headers = map[string]string{"X-Api-Key": "secret-key"}
	m := NewManager("SUI", config.ChainTypeSui, []config.Target{target}, defaultOpts(), nil)

	m.RunOnce(context.Background())

	calls := n.Calls()
	if len(calls) != 1 || calls[0].Method != "sui_getLatestCheckpointSequenceNumber" || calls[0].HTTPMethod != http.MethodPost {
		t.Fatalf("expected one POST sui_getLatestCheckpointSequenceNumber, got %+v", calls)
	}
	if calls[0].Header.Get("X-Api-Key") != "secret-key" {
		t.Error("health check must send the target's headers (API key)")
	}
	if st := m.Status()[0]; !st.Routable || st.BlockNumber != 379726054 {
		t.Errorf("status: %+v", st)
	}
}

func TestSui_HealthCheckFailures(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 503", fakenode.Behavior{HTTPStatus: 503}, "http status 503"},
		{"rate limited", fakenode.Behavior{HTTPStatus: 429}, "http status 429"},
		{"not json", fakenode.Behavior{RawBody: "<html>"}, "invalid json-rpc response"},
		{"json-rpc error", fakenode.Behavior{RPCError: "Method not found"}, "Method not found"},
		{"result is not a number", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":"latest"}`}, "unexpected result"},
		{"dropped connection", fakenode.Behavior{Drop: true}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "PublicNode", config.ChainTypeSui)
			n.Set(tt.b)
			m := NewManager("SUI", config.ChainTypeSui, targetsOf(n), defaultOpts(), nil)

			m.RunOnce(context.Background())

			st := m.Status()[0]
			if st.Routable {
				t.Fatalf("%s must fail the check: %+v", tt.name, st)
			}
			if !strings.Contains(st.LastError, tt.wantErr) {
				t.Errorf("last error %q does not mention %q", st.LastError, tt.wantErr)
			}
		})
	}
}

// Checkpoints advance every ~250ms, so a target that stopped following the
// network must be taken out of rotation like any lagging EVM node.
func TestSui_CheckpointLagBetweenNodes(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeSui)
	b := fakenode.New(t, "B", config.ChainTypeSui)
	a.Set(fakenode.Behavior{Block: 379726054})
	b.Set(fakenode.Behavior{Block: 379725054})
	opts := defaultOpts()
	opts.MaxBlockLag = 100
	m := NewManager("SUI", config.ChainTypeSui, targetsOf(a, b), opts, nil)

	m.RunOnce(context.Background())

	st := m.Status()
	if !st[0].Routable || st[1].Routable || st[1].Lag != 1000 {
		t.Errorf("lag check must work for sui: %+v", st)
	}
}

func TestSui_JSONRPCForwardedVerbatim(t *testing.T) {
	n := fakenode.New(t, "PublicNode", config.ChainTypeSui)
	p := newSuiProxy(t, targetsOf(n), nil, 0)

	rr := post(p, suiEchoBody, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if got := answeredBy(t, rr); got != "PublicNode" {
		t.Errorf("answered by %q", got)
	}
	if rr.Header().Get("X-Rpc-Provider") != "PublicNode" {
		t.Error("provider header missing")
	}
	call := n.Calls()[0]
	if call.Body != suiEchoBody || call.HTTPMethod != http.MethodPost {
		t.Errorf("body not forwarded verbatim: %+v", call)
	}
	// A single-endpoint type ignores the client's path: the target URL is used
	// as configured (providers put API keys in it).
	if call.Path != "/" {
		t.Errorf("path = %q, want the target URL used as-is", call.Path)
	}
	if ev := p.rec.Of(events.KindUpstreamRequest, "PublicNode"); len(ev) != 1 || ev[0].Method != "suix_getAllBalances" {
		t.Errorf("upstream event should name the JSON-RPC method: %+v", ev)
	}
}

func TestSui_TargetURLPathIsKept(t *testing.T) {
	n := fakenode.New(t, "Keyed", config.ChainTypeSui)
	target := n.Target()
	target.HTTPURL = n.URL() + "/v1/abc123"
	p := newSuiProxy(t, []config.Target{target}, nil, 0)

	if rr := post(p, suiEchoBody, nil); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if got := n.Calls()[0].Path; got != "/v1/abc123" {
		t.Errorf("path = %q, want the target's own path (it may carry the API key)", got)
	}
}

func TestSui_HeadersInjectedAndOverrideClient(t *testing.T) {
	n := fakenode.New(t, "Keyed", config.ChainTypeSui)
	target := n.Target()
	target.Headers = map[string]string{"X-Api-Key": "server-key"}
	p := newSuiProxy(t, []config.Target{target}, nil, 0)

	req := httptest.NewRequest(http.MethodPost, "/SUI", strings.NewReader(suiEchoBody))
	req.Header.Set("X-Api-Key", "client-key") // must not leak through
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if got := n.Calls()[0].Header.Get("X-Api-Key"); got != "server-key" {
		t.Errorf("X-Api-Key = %q, want the target's own key", got)
	}
}

// Sui reports a bad request from the client as a JSON-RPC error or, for an
// unknown object, inside the result. Neither says anything about the node, so
// both must reach the client untouched.
func TestSui_ClientErrorsPassThrough(t *testing.T) {
	tests := []struct {
		name string
		body string
		b    fakenode.Behavior
		want string
	}{
		{"invalid params", suiEchoBody, fakenode.Behavior{RPCError: "Invalid params: AccountAddressParseError"}, "AccountAddressParseError"},
		{"unknown object", `{"jsonrpc":"2.0","id":1,"method":"sui_getObject","params":["0xdeadbeef"]}`, fakenode.Behavior{}, `"notExists"`},
		{"http 400", suiEchoBody, fakenode.Behavior{HTTPStatus: 400}, "upstream error 400"},
		{"http 404", suiEchoBody, fakenode.Behavior{HTTPStatus: 404}, "upstream error 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeSui)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", config.ChainTypeSui)
			p := newSuiProxy(t, targetsOf(bad, good), []config.Exception{{Match: "Transient error"}}, time.Second)

			sawBad := false
			for i := 0; i < 20 && !sawBad; i++ {
				rr := post(p, tt.body, nil)
				if rr.Header().Get("X-Rpc-Provider") != "Bad" {
					continue
				}
				sawBad = true
				if !strings.Contains(rr.Body.String(), tt.want) {
					t.Errorf("client error must be passed through verbatim: %d %s", rr.Code, rr.Body.String())
				}
			}
			if !sawBad {
				t.Fatal("Bad was never selected")
			}
			if len(p.rec.Of(events.KindRerouted, "Bad")) != 0 || p.Manager().Status()[0].Tainted {
				t.Error("client-side errors must neither reroute nor taint")
			}
		})
	}
}

func TestSui_NodeSideFailuresFailOver(t *testing.T) {
	tests := []struct {
		name      string
		b         fakenode.Behavior
		wantTaint bool
	}{
		{"exception in the body", fakenode.Behavior{RPCError: "Transient error"}, false},
		{"http 502", fakenode.Behavior{HTTPStatus: 502}, true},
		{"http 429", fakenode.Behavior{HTTPStatus: 429}, true},
		{"http 403 (bad api key)", fakenode.Behavior{HTTPStatus: 403}, false},
		{"dropped connection", fakenode.Behavior{Drop: true}, true},
	}
	exceptions := []config.Exception{{Match: "Transient error"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeSui)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", config.ChainTypeSui)
			p := newSuiProxy(t, targetsOf(bad, good), exceptions, 10*time.Second)

			for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
				rr := post(p, suiEchoBody, nil)
				if rr.Code != http.StatusOK || answeredBy(t, rr) != "Good" {
					t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
				}
			}
			if bad.CallCount("") == 0 {
				t.Fatal("Bad was never selected")
			}
			if len(p.rec.Of(events.KindRerouted, "Bad")) == 0 {
				t.Fatal("expected a reroute away from Bad")
			}
			if got := p.Manager().Status()[0].Tainted; got != tt.wantTaint {
				t.Errorf("tainted = %v, want %v", got, tt.wantTaint)
			}
			for _, c := range good.Calls() {
				if c.Body != suiEchoBody {
					t.Errorf("replayed request corrupted: %+v", c)
				}
			}
		})
	}
}

func TestSui_UpstreamTimeoutFailsOver(t *testing.T) {
	slow := fakenode.New(t, "Slow", config.ChainTypeSui)
	slow.Set(fakenode.Behavior{Hang: true})
	good := fakenode.New(t, "Good", config.ChainTypeSui)
	rec := &events.Recorder{}
	targets := targetsOf(slow, good)
	m := NewManager("SUI", config.ChainTypeSui, targets, defaultOpts(), rec)
	px, err := New(Options{
		Chain: "SUI", Type: config.ChainTypeSui, Targets: targets,
		UpstreamTimeout: 100 * time.Millisecond, Observer: rec,
	}, m)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	for i := 0; i < 20 && slow.CallCount("") == 0; i++ {
		rr := post(px, suiEchoBody, nil)
		if rr.Code != http.StatusOK || answeredBy(t, rr) != "Good" {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
	}
	if slow.CallCount("") == 0 {
		t.Fatal("Slow was never selected")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("upstream_timeout not enforced for sui")
	}
}
