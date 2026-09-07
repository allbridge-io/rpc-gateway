package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

const chainTypeSoroban = config.ChainTypeSoroban

// getHealthBody is what a Soroban client sends: no "params" member at all.
const getHealthBody = `{"jsonrpc":"2.0","id":9,"method":"getHealth"}`

func newSorobanProxy(t *testing.T, targets []config.Target, exceptions []config.Exception, taint time.Duration) *testProxy {
	t.Helper()
	rec := &events.Recorder{}
	clock := newFakeClock()
	opts := defaultOpts()
	opts.TaintDuration = taint
	opts.Now = clock.Now
	m := NewManager("SRB", chainTypeSoroban, targets, opts, rec)
	px, err := New(Options{
		Chain:           "SRB",
		Type:            chainTypeSoroban,
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

func sorobanPost(p http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/SRB", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

func TestSoroban_HealthCheckUsesGetHealth(t *testing.T) {
	n := fakenode.New(t, "SDF", chainTypeSoroban)
	n.Set(fakenode.Behavior{Block: 4501689})
	target := n.Target()
	target.Headers = map[string]string{"X-Api-Key": "secret-key"}
	m := NewManager("SRB", chainTypeSoroban, []config.Target{target}, defaultOpts(), nil)

	m.RunOnce(context.Background())

	calls := n.Calls()
	if len(calls) != 1 || calls[0].Method != "getHealth" || calls[0].HTTPMethod != http.MethodPost {
		t.Fatalf("expected one POST getHealth, got %+v", calls)
	}
	if calls[0].Header.Get("X-Api-Key") != "secret-key" {
		t.Error("health check must send the target's headers (API key)")
	}
	if st := m.Status()[0]; !st.Routable || st.BlockNumber != 4501689 {
		t.Errorf("status: %+v", st)
	}
}

func TestSoroban_HealthCheckFailures(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 503", fakenode.Behavior{HTTPStatus: 503}, "http status 503"},
		{"rate limited", fakenode.Behavior{HTTPStatus: 429}, "http status 429"},
		{"not json", fakenode.Behavior{RawBody: "<html>"}, "invalid json-rpc response"},
		{"json-rpc error", fakenode.Behavior{RPCError: "internal error"}, "internal error"},
		{"status unhealthy", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":{"status":"unhealthy","latestLedger":9}}`}, `status "unhealthy"`},
		{"no latestLedger", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":{"status":"healthy"}}`}, "no latestLedger"},
		{"dropped connection", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "SDF", chainTypeSoroban)
			n.Set(tt.b)
			m := NewManager("SRB", chainTypeSoroban, targetsOf(n), defaultOpts(), nil)

			m.RunOnce(context.Background())

			st := m.Status()[0]
			if st.Routable || !containsAny(st.LastError, tt.wantErr) {
				t.Errorf("%s: %+v", tt.name, st)
			}
		})
	}
}

func TestSoroban_LedgerLagBetweenNodes(t *testing.T) {
	a := fakenode.New(t, "A", chainTypeSoroban)
	b := fakenode.New(t, "B", chainTypeSoroban)
	a.Set(fakenode.Behavior{Block: 4501689})
	b.Set(fakenode.Behavior{Block: 4501679})
	opts := defaultOpts()
	opts.MaxBlockLag = 5
	m := NewManager("SRB", chainTypeSoroban, targetsOf(a, b), opts, nil)

	m.RunOnce(context.Background())

	if st := m.Status(); !st[0].Routable || st[1].Routable || st[1].Lag != 10 {
		t.Errorf("a node 10 ledgers behind must be excluded: %+v", st)
	}
}

func TestSoroban_ForwardsTheJSONRPCBodyVerbatim(t *testing.T) {
	n := fakenode.New(t, "SDF", chainTypeSoroban)
	n.Set(fakenode.Behavior{Block: 4501689})
	p := newSorobanProxy(t, targetsOf(n), nil, 0)

	rr := sorobanPost(p, `{"jsonrpc":"2.0","id":9,"method":"getLatestLedger"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			Sequence uint64 `json:"sequence"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not a soroban response: %s", rr.Body.String())
	}
	if resp.ID != 9 || resp.Result.Sequence != 4501689 {
		t.Errorf("unexpected result %+v", resp)
	}
	if rr.Header().Get("X-Rpc-Provider") != "SDF" {
		t.Error("provider header missing")
	}
	c := n.Calls()[0]
	if c.Body != `{"jsonrpc":"2.0","id":9,"method":"getLatestLedger"}` || c.Path != "/" {
		t.Errorf("body must reach the single endpoint verbatim: %+v", c)
	}
	if ev := p.rec.Of(events.KindUpstreamRequest, "SDF"); len(ev) != 1 || ev[0].Method != "getLatestLedger" {
		t.Errorf("upstream event should name the JSON-RPC method: %+v", ev)
	}
}

func TestSoroban_TargetBasePathIsKept(t *testing.T) {
	n := fakenode.New(t, "SDF", chainTypeSoroban)
	n.Set(fakenode.Behavior{Block: 5})
	target := n.Target()
	target.HTTPURL = n.URL() + "/soroban/rpc" // provider prefixes and API key paths
	p := newSorobanProxy(t, []config.Target{target}, nil, 0)

	if rr := sorobanPost(p, getHealthBody); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if got := n.Calls()[0].Path; got != "/soroban/rpc" {
		t.Errorf("path = %q, want the target's own path", got)
	}
}

func TestSoroban_HeadersInjectedAndOverrideClient(t *testing.T) {
	n := fakenode.New(t, "SDF", chainTypeSoroban)
	target := n.Target()
	target.Headers = map[string]string{"X-Api-Key": "server-key"}
	p := newSorobanProxy(t, []config.Target{target}, nil, 0)

	req := httptest.NewRequest(http.MethodPost, "/SRB", strings.NewReader(getHealthBody))
	req.Header.Set("X-Api-Key", "client-key") // must not leak through
	p.ServeHTTP(httptest.NewRecorder(), req)

	if got := n.Calls()[0].Header.Get("X-Api-Key"); got != "server-key" {
		t.Errorf("X-Api-Key = %q, want the target's own key", got)
	}
}

func TestSoroban_ClientSideErrorsPassThrough(t *testing.T) {
	tests := []struct {
		name string
		body string
		b    fakenode.Behavior
		want string
	}{
		// The real server answers a bad params member with HTTP 200 and -32602.
		{"invalid parameters", `{"jsonrpc":"2.0","id":1,"method":"getHealth","params":[]}`, fakenode.Behavior{}, "invalid parameters"},
		{"json-rpc error", getHealthBody, fakenode.Behavior{RPCError: "method not found"}, "method not found"},
		{"http 400", getHealthBody, fakenode.Behavior{HTTPStatus: 400}, "upstream error 400"},
		{"http 404", getHealthBody, fakenode.Behavior{HTTPStatus: 404}, "upstream error 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", chainTypeSoroban)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", chainTypeSoroban)
			p := newSorobanProxy(t, targetsOf(bad, good), []config.Exception{{Match: "transaction submission failed"}}, time.Second)

			sawBad := false
			for i := 0; i < 20 && !sawBad; i++ {
				rr := sorobanPost(p, tt.body)
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

func TestSoroban_NodeSideFailuresFailOver(t *testing.T) {
	tests := []struct {
		name      string
		b         fakenode.Behavior
		wantTaint bool
	}{
		{"http 500", fakenode.Behavior{HTTPStatus: 500}, true},
		{"http 502", fakenode.Behavior{HTTPStatus: 502}, true},
		{"http 429", fakenode.Behavior{HTTPStatus: 429}, true},
		{"http 403 (bad api key)", fakenode.Behavior{HTTPStatus: 403}, false},
		{"dropped connection", fakenode.Behavior{Drop: true}, true},
		{"configured exception", fakenode.Behavior{RPCError: "transaction submission failed"}, false},
	}
	exceptions := []config.Exception{{Match: "transaction submission failed"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", chainTypeSoroban)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", chainTypeSoroban)
			good.Set(fakenode.Behavior{Block: 4501689})
			p := newSorobanProxy(t, targetsOf(bad, good), exceptions, 10*time.Second)

			for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
				rr := sorobanPost(p, getHealthBody)
				if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"latestLedger":4501689`) {
					t.Fatalf("request %d must be answered by Good: %d %s", i, rr.Code, rr.Body.String())
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
				if c.Body != getHealthBody {
					t.Errorf("replayed request corrupted: %q", c.Body)
				}
			}
		})
	}
}

func TestSoroban_UpstreamTimeoutFailsOver(t *testing.T) {
	slow := fakenode.New(t, "Slow", chainTypeSoroban)
	slow.Set(fakenode.Behavior{Hang: true})
	good := fakenode.New(t, "Good", chainTypeSoroban)
	rec := &events.Recorder{}
	targets := targetsOf(slow, good)
	m := NewManager("SRB", chainTypeSoroban, targets, defaultOpts(), rec)
	px, err := New(Options{
		Chain: "SRB", Type: chainTypeSoroban, Targets: targets,
		UpstreamTimeout: 100 * time.Millisecond, Observer: rec,
	}, m)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	for i := 0; i < 20 && slow.CallCount("") == 0; i++ {
		if rr := sorobanPost(px, getHealthBody); rr.Code != http.StatusOK {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
	}
	if slow.CallCount("") == 0 {
		t.Fatal("Slow was never selected")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("upstream_timeout not enforced for soroban")
	}
	if re := rec.Of(events.KindRerouted, "Slow"); len(re) == 0 || !strings.Contains(re[0].Reason, "timeout") {
		t.Errorf("expected a timeout reroute, got %+v", re)
	}
}
