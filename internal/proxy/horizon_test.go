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

func newHorizonProxy(t *testing.T, targets []config.Target, exceptions []config.Exception, taint time.Duration) *testProxy {
	t.Helper()
	rec := &events.Recorder{}
	clock := newFakeClock()
	opts := defaultOpts()
	opts.TaintDuration = taint
	opts.Now = clock.Now
	m := NewManager("SRB_HORIZON", fakenode.ChainTypeHorizon, targets, opts, rec)
	px, err := New(Options{
		Chain:           "SRB_HORIZON",
		Type:            fakenode.ChainTypeHorizon,
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

// horizonEcho is what fakenode returns for any path it does not simulate.
type horizonEcho struct {
	Node       string `json:"node"`
	HTTPMethod string `json:"httpMethod"`
	Path       string `json:"path"`
	Query      string `json:"query"`
	Body       string `json:"body"`
}

// horizonRequest sends the sub-path the router would hand to the proxy.
func horizonRequest(p http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

func decodeHorizonEcho(t *testing.T, rr *httptest.ResponseRecorder) horizonEcho {
	t.Helper()
	var e horizonEcho
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatalf("not a horizon echo: %d %s", rr.Code, rr.Body.String())
	}
	return e
}

func TestHorizon_HealthCheckReadsTheRootDocument(t *testing.T) {
	n := fakenode.New(t, "SDF", fakenode.ChainTypeHorizon)
	n.Set(fakenode.Behavior{Block: 4501678})
	target := n.Target()
	target.Headers = map[string]string{"X-Api-Key": "secret-key"}
	m := NewManager("SRB_HORIZON", fakenode.ChainTypeHorizon, []config.Target{target}, defaultOpts(), nil)

	m.RunOnce(context.Background())

	calls := n.Calls()
	if len(calls) != 1 || calls[0].Path != "/" || calls[0].HTTPMethod != http.MethodGet {
		t.Fatalf("expected one GET /, got %+v", calls)
	}
	if calls[0].Header.Get("X-Api-Key") != "secret-key" {
		t.Error("health check must send the target's headers (API key)")
	}
	if st := m.Status()[0]; !st.Routable || st.BlockNumber != 4501678 {
		t.Errorf("status: %+v", st)
	}
}

func TestHorizon_HealthCheckFailures(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 503", fakenode.Behavior{HTTPStatus: 503}, "http status 503"},
		{"rate limited", fakenode.Behavior{HTTPStatus: 429}, "http status 429"},
		{"not json", fakenode.Behavior{RawBody: "<html>"}, "invalid response"},
		{"no ledger field", fakenode.Behavior{RawBody: `{"core_latest_ledger":9}`}, "no history_latest_ledger"},
		{"dropped connection", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "SDF", fakenode.ChainTypeHorizon)
			n.Set(tt.b)
			m := NewManager("SRB_HORIZON", fakenode.ChainTypeHorizon, targetsOf(n), defaultOpts(), nil)
			m.RunOnce(context.Background())
			if st := m.Status()[0]; st.Routable || !containsAny(st.LastError, tt.wantErr) {
				t.Errorf("%s: %+v", tt.name, st)
			}
		})
	}
}

// An instance whose ingestion is stuck answers every request happily; only the
// ledger it reports gives it away.
func TestHorizon_LedgerLagBetweenNodes(t *testing.T) {
	a := fakenode.New(t, "A", fakenode.ChainTypeHorizon)
	b := fakenode.New(t, "B", fakenode.ChainTypeHorizon)
	a.Set(fakenode.Behavior{Block: 4501678})
	b.Set(fakenode.Behavior{Block: 4501668})
	opts := defaultOpts()
	opts.MaxBlockLag = 5
	m := NewManager("SRB_HORIZON", fakenode.ChainTypeHorizon, targetsOf(a, b), opts, nil)

	m.RunOnce(context.Background())

	if st := m.Status(); !st[0].Routable || st[1].Routable || st[1].Lag != 10 {
		t.Errorf("lag check must work for horizon: %+v", st)
	}
}

func TestHorizon_PassesPathQueryAndMethod(t *testing.T) {
	n := fakenode.New(t, "SDF", fakenode.ChainTypeHorizon)
	n.Set(fakenode.Behavior{Block: 4501678})
	p := newHorizonProxy(t, targetsOf(n), nil, 0)

	rr := horizonRequest(p, http.MethodGet, "/accounts/GABC/transactions?order=desc&limit=2", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	e := decodeHorizonEcho(t, rr)
	if e.Path != "/accounts/GABC/transactions" || e.HTTPMethod != http.MethodGet || e.Query != "order=desc&limit=2" {
		t.Errorf("GET not passed through: %+v", e)
	}
	if rr.Header().Get("X-Rpc-Provider") != "SDF" {
		t.Error("provider header missing")
	}

	// Horizon submits transactions with a form POST; the body must survive.
	rr = horizonRequest(p, http.MethodPost, "/transactions", "tx=AAAAAgAAAA")
	e = decodeHorizonEcho(t, rr)
	if e.Path != "/transactions" || e.HTTPMethod != http.MethodPost || e.Body != "tx=AAAAAgAAAA" {
		t.Errorf("POST not passed through: %+v", e)
	}

	// The root and the collections the fake node simulates answer like Horizon.
	rr = horizonRequest(p, http.MethodGet, "/", "")
	if !strings.Contains(rr.Body.String(), `"history_latest_ledger":4501678`) {
		t.Errorf("root document not served: %d %s", rr.Code, rr.Body.String())
	}
	rr = horizonRequest(p, http.MethodGet, "/ledgers?order=desc&limit=1", "")
	if !strings.Contains(rr.Body.String(), `"sequence":4501678`) {
		t.Errorf("/ledgers not served: %d %s", rr.Code, rr.Body.String())
	}

	ev := p.rec.Of(events.KindUpstreamRequest, "SDF")
	if len(ev) != 4 || ev[0].Method != "GET /accounts/GABC/transactions" {
		t.Errorf("upstream events should name the path for non JSON-RPC calls: %+v", ev)
	}
}

func TestHorizon_TargetBasePathAndQueryAreKept(t *testing.T) {
	n := fakenode.New(t, "SDF", fakenode.ChainTypeHorizon)
	target := n.Target()
	target.HTTPURL = n.URL() + "/horizon/?apikey=k1"
	p := newHorizonProxy(t, []config.Target{target}, nil, 0)

	rr := horizonRequest(p, http.MethodGet, "/accounts/GABC?limit=1", "")
	e := decodeHorizonEcho(t, rr)
	if e.Path != "/horizon/accounts/GABC" || e.Query != "apikey=k1&limit=1" {
		t.Errorf("base path/query must be merged with the client's: %+v", e)
	}
}

func TestHorizon_HeadersInjectedAndOverrideClient(t *testing.T) {
	n := fakenode.New(t, "SDF", fakenode.ChainTypeHorizon)
	target := n.Target()
	target.Headers = map[string]string{"X-Api-Key": "server-key", "X-Extra": "1"}
	p := newHorizonProxy(t, []config.Target{target}, nil, 0)

	req := httptest.NewRequest(http.MethodGet, "/ledgers?limit=1", nil)
	req.Header.Set("X-Api-Key", "client-key") // must not leak through
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	h := n.Calls()[0].Header
	if h.Get("X-Api-Key") != "server-key" || h.Get("X-Extra") != "1" {
		t.Errorf("headers not injected: %v", h)
	}
}

// Horizon answers a malformed or unknown resource with problem+json and a 4xx.
// That is the client's mistake, not a broken provider: it must reach the client
// unchanged, without a reroute and without tainting the target.
func TestHorizon_ClientErrorsPassThrough(t *testing.T) {
	tests := []struct {
		name string
		b    fakenode.Behavior
		want string
	}{
		{
			"400 invalid account id",
			fakenode.HorizonProblem(400, "bad_request", "Bad Request", "The request you sent was invalid in some way."),
			"bad_request",
		},
		{
			"404 unknown account",
			fakenode.HorizonProblem(404, "not_found", "Resource Missing", "The resource at the url requested was not found."),
			"not_found",
		},
		{
			"406 before_history",
			fakenode.HorizonProblem(406, "before_history", "Data Requested Is Before Recorded History", "not in the recorded history range"),
			"before_history",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", fakenode.ChainTypeHorizon)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", fakenode.ChainTypeHorizon)
			p := newHorizonProxy(t, targetsOf(bad, good), nil, time.Second)

			sawBad := false
			for i := 0; i < 20 && !sawBad; i++ {
				rr := horizonRequest(p, http.MethodGet, "/accounts/GINVALID", "")
				if rr.Header().Get("X-Rpc-Provider") != "Bad" {
					continue
				}
				sawBad = true
				if rr.Code != tt.b.HTTPStatus || !strings.Contains(rr.Body.String(), tt.want) {
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

func TestHorizon_NodeSideFailuresFailOver(t *testing.T) {
	tests := []struct {
		name string
		b    fakenode.Behavior
	}{
		{"http 500", fakenode.HorizonProblem(500, "server_error", "Internal Server Error", "An error occurred while processing this request.")},
		{"http 503 stale history", fakenode.HorizonProblem(503, "stale_history", "Historical DB Is Too Stale", "the history database is lagging")},
		{"http 429 rate limited", fakenode.HorizonProblem(429, "rate_limit_exceeded", "Rate limit exceeded", "too many requests")},
		{"dropped connection", fakenode.Behavior{Drop: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", fakenode.ChainTypeHorizon)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", fakenode.ChainTypeHorizon)
			p := newHorizonProxy(t, targetsOf(bad, good), nil, 10*time.Second)

			for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
				rr := horizonRequest(p, http.MethodGet, "/ledgers?limit=1", "")
				if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"records"`) {
					t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
				}
				if rr.Header().Get("X-Rpc-Provider") != "Good" && bad.CallCount("") > 0 {
					t.Errorf("the answer must come from Good, header says %q", rr.Header().Get("X-Rpc-Provider"))
				}
			}
			if bad.CallCount("") == 0 {
				t.Fatal("Bad was never selected")
			}
			if len(p.rec.Of(events.KindRerouted, "Bad")) == 0 {
				t.Fatal("expected a reroute away from Bad")
			}
			if !p.Manager().Status()[0].Tainted {
				t.Error("a failing provider must be tainted so the next requests skip it")
			}
		})
	}
}

func TestHorizon_UpstreamTimeoutFailsOver(t *testing.T) {
	slow := fakenode.New(t, "Slow", fakenode.ChainTypeHorizon)
	slow.Set(fakenode.Behavior{Hang: true})
	good := fakenode.New(t, "Good", fakenode.ChainTypeHorizon)
	rec := &events.Recorder{}
	targets := targetsOf(slow, good)
	m := NewManager("SRB_HORIZON", fakenode.ChainTypeHorizon, targets, defaultOpts(), rec)
	px, err := New(Options{
		Chain:           "SRB_HORIZON",
		Type:            fakenode.ChainTypeHorizon,
		Targets:         targets,
		UpstreamTimeout: 100 * time.Millisecond,
		Observer:        rec,
	}, m)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	for i := 0; i < 20 && slow.CallCount("") == 0; i++ {
		rr := horizonRequest(px, http.MethodGet, "/ledgers?limit=1", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
	}
	if slow.CallCount("") == 0 {
		t.Fatal("Slow was never selected")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("upstream_timeout not enforced for horizon")
	}
}
