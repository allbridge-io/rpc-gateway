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

func newTonProxy(t *testing.T, targets []config.Target, taint time.Duration) *testProxy {
	t.Helper()
	rec := &events.Recorder{}
	clock := newFakeClock()
	opts := defaultOpts()
	opts.TaintDuration = taint
	opts.Now = clock.Now
	m := NewManager("TON", config.ChainTypeTON, targets, opts, rec)
	px, err := New(Options{
		Chain:           "TON",
		Type:            config.ChainTypeTON,
		Targets:         targets,
		UpstreamTimeout: 2 * time.Second,
		Observer:        rec,
	}, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testProxy{Proxy: px, rec: rec, clock: clock}
}

// tonEcho is what fakenode returns for any toncenter path it does not simulate.
type tonEcho struct {
	Node       string `json:"node"`
	HTTPMethod string `json:"httpMethod"`
	Path       string `json:"path"`
	Query      string `json:"query"`
	Body       string `json:"body"`
}

func tonRequest(p http.Handler, method, path, body string) *httptest.ResponseRecorder {
	// The router hands the proxy the sub-path after /TON.
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

func decodeTonEcho(t *testing.T, rr *httptest.ResponseRecorder) tonEcho {
	t.Helper()
	var e tonEcho
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatalf("not a ton echo: %d %s", rr.Code, rr.Body.String())
	}
	return e
}

func TestTON_HealthCheckUsesMasterchainInfo(t *testing.T) {
	n := fakenode.New(t, "Toncenter", config.ChainTypeTON)
	n.Set(fakenode.Behavior{Block: 82614017})
	target := n.Target()
	target.Headers = map[string]string{"X-API-Key": "secret-key"}
	m := NewManager("TON", config.ChainTypeTON, []config.Target{target}, defaultOpts(), nil)

	m.RunOnce(context.Background())

	calls := n.Calls()
	if len(calls) != 1 || calls[0].Path != "/api/v3/masterchainInfo" || calls[0].HTTPMethod != http.MethodGet {
		t.Fatalf("expected one GET /api/v3/masterchainInfo, got %+v", calls)
	}
	if calls[0].Header.Get("X-API-Key") != "secret-key" {
		t.Error("health check must send the target's headers (toncenter API key)")
	}
	if st := m.Status()[0]; !st.Routable || st.BlockNumber != 82614017 {
		t.Errorf("status: %+v", st)
	}
}

func TestTON_HealthCheckFailures(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 503", fakenode.Behavior{HTTPStatus: 503}, "http status 503"},
		{"rate limited", fakenode.Behavior{HTTPStatus: 429, RawBody: `{"error":"Rate limit exceeded"}`}, "http status 429"},
		{"toncenter error body", fakenode.Behavior{RawBody: `{"error":"lite server timeout"}`}, "lite server timeout"},
		{"not json", fakenode.Behavior{RawBody: "<html>"}, "invalid response"},
		{"no seqno", fakenode.Behavior{RawBody: `{"last":{"workchain":-1}}`}, "no last.seqno"},
		{"dropped connection", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "Toncenter", config.ChainTypeTON)
			n.Set(tt.b)
			m := NewManager("TON", config.ChainTypeTON, targetsOf(n), defaultOpts(), nil)
			m.RunOnce(context.Background())
			st := m.Status()[0]
			if st.Routable || !containsAny(st.LastError, tt.wantErr) {
				t.Errorf("%s: %+v", tt.name, st)
			}
		})
	}
}

func TestTON_BlockLagBetweenNodes(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeTON)
	b := fakenode.New(t, "B", config.ChainTypeTON)
	a.Set(fakenode.Behavior{Block: 82614017})
	b.Set(fakenode.Behavior{Block: 82614007})
	opts := defaultOpts()
	opts.MaxBlockLag = 5
	m := NewManager("TON", config.ChainTypeTON, targetsOf(a, b), opts, nil)

	m.RunOnce(context.Background())

	if st := m.Status(); !st[0].Routable || st[1].Routable || st[1].Lag != 10 {
		t.Errorf("lag check must work for ton: %+v", st)
	}
}

func TestTON_PassesPathQueryMethodAndBody(t *testing.T) {
	n := fakenode.New(t, "Toncenter", config.ChainTypeTON)
	p := newTonProxy(t, targetsOf(n), 0)

	rr := tonRequest(p, http.MethodGet, "/api/v3/blocks?limit=1&sort=desc", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	e := decodeTonEcho(t, rr)
	if e.Path != "/api/v3/blocks" || e.HTTPMethod != http.MethodGet || e.Query != "limit=1&sort=desc" {
		t.Errorf("GET with query not passed through: %+v", e)
	}
	if rr.Header().Get("X-Rpc-Provider") != "Toncenter" {
		t.Error("provider header missing")
	}

	body := `{"boc":"te6cckEBAQEAAgAAAEysuc0="}`
	rr = tonRequest(p, http.MethodPost, "/api/v3/message", body)
	e = decodeTonEcho(t, rr)
	if e.Path != "/api/v3/message" || e.HTTPMethod != http.MethodPost || e.Body != body {
		t.Errorf("POST not passed through: %+v", e)
	}
	ev := p.rec.Of(events.KindUpstreamRequest, "Toncenter")
	if len(ev) != 2 || ev[0].Method != "GET /api/v3/blocks" {
		t.Errorf("upstream events should name the path for non JSON-RPC calls: %+v", ev)
	}
}

func TestTON_TargetBasePathAndQueryAreKept(t *testing.T) {
	n := fakenode.New(t, "Toncenter", config.ChainTypeTON)
	target := n.Target()
	target.HTTPURL = n.URL() + "/base/?api_key=k1"
	p := newTonProxy(t, []config.Target{target}, 0)

	rr := tonRequest(p, http.MethodGet, "/api/v3/blocks?limit=1", "")
	e := decodeTonEcho(t, rr)
	if e.Path != "/base/api/v3/blocks" || e.Query != "api_key=k1&limit=1" {
		t.Errorf("base path/query must be merged with the client's: %+v", e)
	}
}

func TestTON_HeadersInjectedAndOverrideClient(t *testing.T) {
	n := fakenode.New(t, "Toncenter", config.ChainTypeTON)
	target := n.Target()
	target.Headers = map[string]string{"X-API-Key": "server-key", "X-Extra": "1"}
	p := newTonProxy(t, []config.Target{target}, 0)

	req := httptest.NewRequest(http.MethodGet, "/api/v3/addressInformation?address=EQAbc", nil)
	req.Header.Set("X-API-Key", "client-key") // must not leak through
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	h := n.Calls()[0].Header
	if h.Get("X-API-Key") != "server-key" || h.Get("X-Extra") != "1" {
		t.Errorf("headers not injected: %v", h)
	}
}

// The backend also talks to toncenter's JSON-RPC endpoint; it must reach the
// target through the same chain prefix as the v3 REST API.
func TestTON_JSONRPCThroughSamePrefix(t *testing.T) {
	n := fakenode.New(t, "Toncenter", config.ChainTypeTON)
	n.Set(fakenode.Behavior{Block: 82614017})
	p := newTonProxy(t, targetsOf(n), 0)

	rr := tonRequest(p, http.MethodPost, "/api/v2/jsonRPC", `{"jsonrpc":"2.0","id":1,"method":"getMasterchainInfo","params":{}}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"seqno":82614017`) {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if c := n.Calls()[0]; c.Path != "/api/v2/jsonRPC" || c.Method != "getMasterchainInfo" {
		t.Errorf("jsonrpc call not passed: %+v", c)
	}
	if ev := p.rec.Of(events.KindUpstreamRequest, "Toncenter"); len(ev) != 1 || ev[0].Method != "getMasterchainInfo" {
		t.Errorf("a JSON-RPC body must be reported by method name: %+v", ev)
	}
}

func TestTON_ClientSideErrorsPassThrough(t *testing.T) {
	tests := []struct {
		name string
		b    fakenode.Behavior
		want string
	}{
		{"422 bad address", fakenode.Behavior{HTTPStatus: 422, RawBody: `{"error":"failed to decode: schema"}`}, "failed to decode"},
		{"404 unknown route", fakenode.Behavior{HTTPStatus: 404, RawBody: `{"error":"Not Found"}`}, "Not Found"},
		{"400 bad request", fakenode.Behavior{HTTPStatus: 400, RawBody: `{"error":"invalid limit"}`}, "invalid limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeTON)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", config.ChainTypeTON)
			p := newTonProxy(t, targetsOf(bad, good), time.Second)

			sawBad := false
			for i := 0; i < 20 && !sawBad; i++ {
				rr := tonRequest(p, http.MethodGet, "/api/v3/addressInformation?address=bad", "")
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

func TestTON_NodeSideFailuresFailOver(t *testing.T) {
	tests := []struct {
		name  string
		b     fakenode.Behavior
		taint bool // 401/403 may be caused by a forwarded client header: reroute only
	}{
		// toncenter answers 429 once the free rate limit (~1 rps) is exceeded:
		// the target must be taken out of rotation, not returned to the client.
		{"http 429 rate limited", fakenode.Behavior{HTTPStatus: 429, RawBody: `{"error":"Rate limit exceeded"}`}, true},
		{"http 502", fakenode.Behavior{HTTPStatus: 502}, true},
		{"http 503", fakenode.Behavior{HTTPStatus: 503, RawBody: `{"error":"lite server timeout"}`}, true},
		{"http 401 (bad api key)", fakenode.Behavior{HTTPStatus: 401, RawBody: `{"error":"invalid api key"}`}, false},
		{"dropped connection", fakenode.Behavior{Drop: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeTON)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", config.ChainTypeTON)
			p := newTonProxy(t, targetsOf(bad, good), 10*time.Second)

			for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
				rr := tonRequest(p, http.MethodGet, "/api/v3/blocks?limit=1", "")
				if rr.Code != http.StatusOK || decodeTonEcho(t, rr).Node != "Good" {
					t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
				}
			}
			if bad.CallCount("") == 0 {
				t.Fatal("Bad was never selected")
			}
			if len(p.rec.Of(events.KindRerouted, "Bad")) == 0 {
				t.Fatal("expected a reroute away from Bad")
			}
			if got := p.Manager().Status()[0].Tainted; got != tt.taint {
				t.Errorf("tainted = %v, want %v", got, tt.taint)
			}
			for _, c := range good.Calls() {
				if c.Path != "/api/v3/blocks" || c.Query != "limit=1" {
					t.Errorf("replayed request corrupted: %+v", c)
				}
			}
		})
	}
}

// A POST body must survive the reroute, or a message broadcast would be lost.
func TestTON_PostBodyReplayedOnFailover(t *testing.T) {
	bad := fakenode.New(t, "Bad", config.ChainTypeTON)
	bad.Set(fakenode.Behavior{HTTPStatus: 429, RawBody: `{"error":"Rate limit exceeded"}`})
	good := fakenode.New(t, "Good", config.ChainTypeTON)
	p := newTonProxy(t, targetsOf(bad, good), 0)

	const body = `{"boc":"te6cckEBAQEAAgAAAEysuc0="}`
	for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
		rr := tonRequest(p, http.MethodPost, "/api/v3/message", body)
		if rr.Code != http.StatusOK || decodeTonEcho(t, rr).Node != "Good" {
			t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	if bad.CallCount("") == 0 {
		t.Fatal("Bad was never selected")
	}
	for _, c := range good.Calls() {
		if c.Body != body {
			t.Errorf("replayed body corrupted: %q", c.Body)
		}
	}
}

func TestTON_UpstreamTimeoutFailsOver(t *testing.T) {
	slow := fakenode.New(t, "Slow", config.ChainTypeTON)
	slow.Set(fakenode.Behavior{Hang: true})
	good := fakenode.New(t, "Good", config.ChainTypeTON)
	rec := &events.Recorder{}
	targets := targetsOf(slow, good)
	m := NewManager("TON", config.ChainTypeTON, targets, defaultOpts(), rec)
	px, err := New(Options{
		Chain: "TON", Type: config.ChainTypeTON, Targets: targets,
		UpstreamTimeout: 100 * time.Millisecond, Observer: rec,
	}, m)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 20 && slow.CallCount("") == 0; i++ {
		rr := tonRequest(px, http.MethodGet, "/api/v3/blocks?limit=1", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
	}
	if slow.CallCount("") == 0 {
		t.Fatal("Slow was never selected")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("upstream_timeout not enforced for ton")
	}
}
