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

// suiEchoQuery is a GraphQL request the fake node does not simulate, so the
// answer is an echo naming the node that served it.
const suiEchoQuery = `{"query":"{ object(address: \"0x2\") { version } }"}`

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

// suiGraphQL sends a GraphQL query to the sub-path the router hands to the proxy.
func suiGraphQL(p http.Handler, body string) *httptest.ResponseRecorder {
	return horizonRequest(p, http.MethodPost, "/graphql", body)
}

func TestSui_HealthCheckUsesTheLatestCheckpoint(t *testing.T) {
	n := fakenode.New(t, "QuickNode", config.ChainTypeSui)
	n.Set(fakenode.Behavior{Block: 336755149})
	target := n.Target()
	target.Headers = map[string]string{"X-Api-Key": "secret-key"}
	m := NewManager("SUI", config.ChainTypeSui, []config.Target{target}, defaultOpts(), nil)

	m.RunOnce(context.Background())

	calls := n.Calls()
	if len(calls) != 1 || calls[0].Path != "/graphql" || calls[0].HTTPMethod != http.MethodPost {
		t.Fatalf("expected one POST /graphql, got %+v", calls)
	}
	if calls[0].Header.Get("X-Api-Key") != "secret-key" {
		t.Error("health check must send the target's headers (API key)")
	}
	if st := m.Status()[0]; !st.Routable || st.BlockNumber != 336755149 {
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
		{"json-rpc only (404)", fakenode.Behavior{HTTPStatus: 404, RawBody: " "}, "http status 404"},
		{"not json", fakenode.Behavior{RawBody: "<html>"}, "invalid response"},
		{"graphql error", fakenode.Behavior{RawBody: `{"data":null,"errors":[{"message":"Internal error"}]}`}, "graphql error: Internal error"},
		{"dropped connection", fakenode.Behavior{Drop: true}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "QuickNode", config.ChainTypeSui)
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
	a.Set(fakenode.Behavior{Block: 336755149})
	b.Set(fakenode.Behavior{Block: 336754149})
	opts := defaultOpts()
	opts.MaxBlockLag = 100
	m := NewManager("SUI", config.ChainTypeSui, targetsOf(a, b), opts, nil)

	m.RunOnce(context.Background())

	st := m.Status()
	if !st[0].Routable || st[1].Routable || st[1].Lag != 1000 {
		t.Errorf("lag check must work for sui: %+v", st)
	}
}

func TestSui_GraphQLForwardedVerbatim(t *testing.T) {
	n := fakenode.New(t, "QuickNode", config.ChainTypeSui)
	p := newSuiProxy(t, targetsOf(n), nil, 0)

	rr := suiGraphQL(p, suiEchoQuery)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	echo := decodeHorizonEcho(t, rr)
	if echo.Node != "QuickNode" || echo.Path != "/graphql" || echo.HTTPMethod != http.MethodPost || echo.Body != suiEchoQuery {
		t.Errorf("query not forwarded verbatim to /graphql: %+v", echo)
	}
	if rr.Header().Get("X-Rpc-Provider") != "QuickNode" {
		t.Error("provider header missing")
	}
	if ev := p.rec.Of(events.KindUpstreamRequest, "QuickNode"); len(ev) != 1 || ev[0].Method != "POST /graphql" {
		t.Errorf("upstream event should name the HTTP method and path: %+v", ev)
	}
}

// QuickNode keeps the token in the path: the client's sub-path goes under it,
// and so does the bare chain root (JSON-RPC clients posting to /SUI keep
// reaching whatever the provider still serves there).
func TestSui_TargetBasePathIsKept(t *testing.T) {
	n := fakenode.New(t, "Keyed", config.ChainTypeSui)
	target := n.Target()
	target.HTTPURL = n.URL() + "/abc123/"
	p := newSuiProxy(t, []config.Target{target}, nil, 0)

	for path, want := range map[string]string{"/graphql": "/abc123/graphql", "/": "/abc123/"} {
		rr := horizonRequest(p, http.MethodPost, path, suiEchoQuery)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		if got := decodeHorizonEcho(t, rr).Path; got != want {
			t.Errorf("%s: path = %q, want %q (the target's own path may carry the API key)", path, got, want)
		}
	}
}

func TestSui_HeadersInjectedAndOverrideClient(t *testing.T) {
	n := fakenode.New(t, "Keyed", config.ChainTypeSui)
	target := n.Target()
	target.Headers = map[string]string{"X-Api-Key": "server-key"}
	p := newSuiProxy(t, []config.Target{target}, nil, 0)

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(suiEchoQuery))
	req.Header.Set("X-Api-Key", "client-key") // must not leak through
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if got := n.Calls()[0].Header.Get("X-Api-Key"); got != "server-key" {
		t.Errorf("X-Api-Key = %q, want the target's own key", got)
	}
}

// GraphQL reports a bad query from the client as HTTP 200 with "errors" (or a
// 400 for a body that is not a GraphQL request at all). Neither says anything
// about the node, so both must reach the client untouched.
func TestSui_ClientErrorsPassThrough(t *testing.T) {
	const validation = `{"data":null,"errors":[{"message":"Unknown field \"nope\" on type \"Query\".","extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`
	tests := []struct {
		name string
		b    fakenode.Behavior
		want string
	}{
		{"graphql validation error", fakenode.Behavior{RawBody: validation}, "GRAPHQL_VALIDATION_FAILED"},
		{"http 400", fakenode.Behavior{HTTPStatus: 400}, "upstream error 400"},
		{"http 404", fakenode.Behavior{HTTPStatus: 404}, "upstream error 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeSui)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", config.ChainTypeSui)
			p := newSuiProxy(t, targetsOf(bad, good), []config.Exception{{Match: "Transient error"}}, time.Second)

			sawBad := false
			for i := 0; i < 20 && !sawBad; i++ {
				rr := suiGraphQL(p, suiEchoQuery)
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
		{"exception in the body", fakenode.Behavior{RawBody: `{"data":null,"errors":[{"message":"Transient error"}]}`}, false},
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
				rr := suiGraphQL(p, suiEchoQuery)
				if rr.Code != http.StatusOK || decodeHorizonEcho(t, rr).Node != "Good" {
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
				if c.Body != suiEchoQuery || c.Path != "/graphql" {
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
		rr := suiGraphQL(px, suiEchoQuery)
		if rr.Code != http.StatusOK || decodeHorizonEcho(t, rr).Node != "Good" {
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
