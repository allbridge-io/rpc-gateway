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

// hiroBadRequest is how the Hiro API rejects a malformed principal: a real 4xx
// status with a JSON body of its own shape.
const hiroBadRequest = `{"statusCode":400,"code":"FST_ERR_VALIDATION","error":"Bad Request","message":"params/principal must match pattern"}`

func newStacksProxy(t *testing.T, targets []config.Target, exceptions []config.Exception, taint time.Duration) *testProxy {
	t.Helper()
	rec := &events.Recorder{}
	clock := newFakeClock()
	opts := defaultOpts()
	opts.TaintDuration = taint
	opts.Now = clock.Now
	m := NewManager("STX", config.ChainTypeStacks, targets, opts, rec)
	px, err := New(Options{
		Chain:           "STX",
		Type:            config.ChainTypeStacks,
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

// stacksEcho is what fakenode returns for any Stacks path other than /v2/info.
type stacksEcho struct {
	Node       string `json:"node"`
	HTTPMethod string `json:"httpMethod"`
	Path       string `json:"path"`
	Query      string `json:"query"`
	Body       string `json:"body"`
}

// stacksRequest hands the proxy the sub-path after /STX, the way the router does.
func stacksRequest(p http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

func decodeStacksEcho(t *testing.T, rr *httptest.ResponseRecorder) stacksEcho {
	t.Helper()
	var e stacksEcho
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatalf("not a stacks echo: %d %s", rr.Code, rr.Body.String())
	}
	return e
}

func TestStacks_HealthCheckUsesV2Info(t *testing.T) {
	n := fakenode.New(t, "Hiro", config.ChainTypeStacks)
	n.Set(fakenode.Behavior{Block: 256844})
	target := n.Target()
	target.Headers = map[string]string{"x-api-key": "secret-key"}
	m := NewManager("STX", config.ChainTypeStacks, []config.Target{target}, defaultOpts(), nil)

	m.RunOnce(context.Background())

	calls := n.Calls()
	if len(calls) != 1 || calls[0].Path != "/v2/info" || calls[0].HTTPMethod != http.MethodGet {
		t.Fatalf("expected one GET /v2/info, got %+v", calls)
	}
	if calls[0].Header.Get("x-api-key") != "secret-key" {
		t.Error("health check must send the target's headers (API key)")
	}
	if st := m.Status()[0]; !st.Routable || st.BlockNumber != 256844 {
		t.Errorf("status: %+v", st)
	}
}

func TestStacks_HealthCheckFailures(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 503", fakenode.Behavior{HTTPStatus: 503}, "http status 503"},
		{"hiro 429 body", fakenode.Behavior{HTTPStatus: 429, RawBody: `{"statusCode":429,"error":"Too Many Requests"}`}, "http status 429"},
		{"not json", fakenode.Behavior{RawBody: "<html>"}, "invalid response"},
		{"no stacks_tip_height", fakenode.Behavior{RawBody: `{"network_id":1}`}, "no stacks_tip_height"},
		{"dropped connection", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "Hiro", config.ChainTypeStacks)
			n.Set(tt.b)
			m := NewManager("STX", config.ChainTypeStacks, targetsOf(n), defaultOpts(), nil)
			m.RunOnce(context.Background())
			st := m.Status()[0]
			if st.Routable || !containsAny(st.LastError, tt.wantErr) {
				t.Errorf("%s: %+v", tt.name, st)
			}
		})
	}
}

func TestStacks_BlockLagBetweenNodes(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeStacks)
	b := fakenode.New(t, "B", config.ChainTypeStacks)
	a.Set(fakenode.Behavior{Block: 256844})
	b.Set(fakenode.Behavior{Block: 256834})
	opts := defaultOpts()
	opts.MaxBlockLag = 5
	m := NewManager("STX", config.ChainTypeStacks, targetsOf(a, b), opts, nil)

	m.RunOnce(context.Background())

	if st := m.Status(); !st[0].Routable || st[1].Routable || st[1].Lag != 10 {
		t.Errorf("lag check must work for stacks: %+v", st)
	}
}

func TestStacks_PassesPathQueryMethodAndBody(t *testing.T) {
	n := fakenode.New(t, "Hiro", config.ChainTypeStacks)
	p := newStacksProxy(t, targetsOf(n), nil, 0)

	rr := stacksRequest(p, http.MethodGet, "/extended/v1/block?limit=1&offset=0", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	e := decodeStacksEcho(t, rr)
	if e.Path != "/extended/v1/block" || e.HTTPMethod != http.MethodGet || e.Query != "limit=1&offset=0" {
		t.Errorf("GET with query not passed through: %+v", e)
	}
	if rr.Header().Get("X-Rpc-Provider") != "Hiro" {
		t.Error("provider header missing")
	}

	// Broadcasting a transaction is a POST with a raw body to the node endpoint.
	rr = stacksRequest(p, http.MethodPost, "/v2/transactions", `{"tx":"80800000"}`)
	e = decodeStacksEcho(t, rr)
	if e.Path != "/v2/transactions" || e.HTTPMethod != http.MethodPost || e.Body != `{"tx":"80800000"}` {
		t.Errorf("POST not passed through: %+v", e)
	}

	ev := p.rec.Of(events.KindUpstreamRequest, "Hiro")
	if len(ev) != 2 || ev[0].Method != "GET /extended/v1/block" {
		t.Errorf("upstream events should name the path for non JSON-RPC calls: %+v", ev)
	}
}

func TestStacks_TargetBasePathAndQueryAreKept(t *testing.T) {
	n := fakenode.New(t, "Hiro", config.ChainTypeStacks)
	target := n.Target()
	target.HTTPURL = n.URL() + "/stacks/?apikey=k1"
	p := newStacksProxy(t, []config.Target{target}, nil, 0)

	rr := stacksRequest(p, http.MethodGet, "/extended/v1/block?limit=1", "")
	e := decodeStacksEcho(t, rr)
	if e.Path != "/stacks/extended/v1/block" || e.Query != "apikey=k1&limit=1" {
		t.Errorf("base path/query must be merged with the client's: %+v", e)
	}
}

func TestStacks_HeadersInjectedAndOverrideClient(t *testing.T) {
	n := fakenode.New(t, "Hiro", config.ChainTypeStacks)
	target := n.Target()
	target.Headers = map[string]string{"x-api-key": "server-key", "X-Extra": "1"}
	p := newStacksProxy(t, []config.Target{target}, nil, 0)

	req := httptest.NewRequest(http.MethodGet, "/extended/v1/status", nil)
	req.Header.Set("x-api-key", "client-key") // must not leak through
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	h := n.Calls()[0].Header
	if h.Get("x-api-key") != "server-key" || h.Get("X-Extra") != "1" {
		t.Errorf("headers not injected: %v", h)
	}
}

// Hiro answers a bad request or an unknown resource with a 4xx and a JSON body.
// That is the client's problem, not the provider's: it must reach the client
// untouched, without a reroute and without tainting the target.
func TestStacks_ClientErrorsPassThrough(t *testing.T) {
	tests := []struct {
		name string
		b    fakenode.Behavior
		want string
	}{
		{"400 bad principal", fakenode.Behavior{HTTPStatus: 400, RawBody: hiroBadRequest}, "FST_ERR_VALIDATION"},
		{"404 unknown tx", fakenode.Behavior{HTTPStatus: 404, RawBody: `{"error":"cannot find transaction"}`}, "cannot find transaction"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeStacks)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", config.ChainTypeStacks)
			p := newStacksProxy(t, targetsOf(bad, good), nil, time.Second)

			sawBad := false
			for i := 0; i < 20 && !sawBad; i++ {
				rr := stacksRequest(p, http.MethodGet, "/extended/v1/address/SPINVALID/balances", "")
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

func TestStacks_NodeSideFailuresFailOver(t *testing.T) {
	tests := []struct {
		name string
		b    fakenode.Behavior
	}{
		{"http 502", fakenode.Behavior{HTTPStatus: 502}},
		{"http 429 rate limited", fakenode.Behavior{HTTPStatus: 429, RawBody: `{"statusCode":429,"error":"Too Many Requests"}`}},
		{"http 401 (bad api key)", fakenode.Behavior{HTTPStatus: 401}},
		{"dropped connection", fakenode.Behavior{Drop: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeStacks)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", config.ChainTypeStacks)
			p := newStacksProxy(t, targetsOf(bad, good), nil, 10*time.Second)

			for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
				rr := stacksRequest(p, http.MethodPost, "/v2/transactions", `{"tx":"80800000"}`)
				if rr.Code != http.StatusOK || decodeStacksEcho(t, rr).Node != "Good" {
					t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
				}
			}
			if bad.CallCount("") == 0 {
				t.Fatal("Bad was never selected")
			}
			if len(p.rec.Of(events.KindRerouted, "Bad")) == 0 {
				t.Fatal("expected a reroute away from Bad")
			}
			if !p.Manager().Status()[0].Tainted {
				t.Error("a provider-side failure must taint the target")
			}
			for _, c := range good.Calls() {
				if c.Body != `{"tx":"80800000"}` || c.Path != "/v2/transactions" {
					t.Errorf("replayed request corrupted: %+v", c)
				}
			}
		})
	}
}
