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

func newTronProxy(t *testing.T, targets []config.Target, exceptions []config.Exception, taint time.Duration) *testProxy {
	t.Helper()
	rec := &events.Recorder{}
	clock := newFakeClock()
	opts := defaultOpts()
	opts.TaintDuration = taint
	opts.Now = clock.Now
	m := NewManager("TRX", config.ChainTypeTron, targets, opts, rec)
	px, err := New(Options{
		Chain:           "TRX",
		Type:            config.ChainTypeTron,
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

// tronEcho is what fakenode returns for any non-getnowblock Tron path.
type tronEcho struct {
	Node       string `json:"node"`
	HTTPMethod string `json:"httpMethod"`
	Path       string `json:"path"`
	Query      string `json:"query"`
	Body       string `json:"body"`
}

func tronRequest(p http.Handler, method, path, body string) *httptest.ResponseRecorder {
	// The router hands the proxy the sub-path after /TRX.
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

func decodeEcho(t *testing.T, rr *httptest.ResponseRecorder) tronEcho {
	t.Helper()
	var e tronEcho
	if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
		t.Fatalf("not a tron echo: %d %s", rr.Code, rr.Body.String())
	}
	return e
}

func TestTron_HealthCheckUsesGetNowBlock(t *testing.T) {
	n := fakenode.New(t, "Grid", config.ChainTypeTron)
	n.Set(fakenode.Behavior{Block: 68000000})
	target := n.Target()
	target.Headers = map[string]string{"TRON-PRO-API-KEY": "secret-key"}
	m := NewManager("TRX", config.ChainTypeTron, []config.Target{target}, defaultOpts(), nil)

	m.RunOnce(context.Background())

	calls := n.Calls()
	if len(calls) != 1 || calls[0].Path != "/wallet/getnowblock" || calls[0].HTTPMethod != http.MethodPost {
		t.Fatalf("expected one POST /wallet/getnowblock, got %+v", calls)
	}
	if calls[0].Header.Get("TRON-PRO-API-KEY") != "secret-key" {
		t.Error("health check must send the target's headers (API key)")
	}
	if st := m.Status()[0]; !st.Routable || st.BlockNumber != 68000000 {
		t.Errorf("status: %+v", st)
	}
}

func TestTron_HealthCheckFailures(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 503", fakenode.Behavior{HTTPStatus: 503}, "http status 503"},
		{"tron Error body", fakenode.Behavior{TronError: "class java.lang.OutOfMemoryError"}, "OutOfMemoryError"},
		{"not json", fakenode.Behavior{RawBody: "<html>"}, "invalid response"},
		{"no block number", fakenode.Behavior{RawBody: `{"blockID":"x"}`}, "no block number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "Grid", config.ChainTypeTron)
			n.Set(tt.b)
			m := NewManager("TRX", config.ChainTypeTron, targetsOf(n), defaultOpts(), nil)
			m.RunOnce(context.Background())
			if st := m.Status()[0]; st.Routable || !strings.Contains(st.LastError, tt.wantErr) {
				t.Errorf("%s: %+v", tt.name, st)
			}
		})
	}
}

func TestTron_BlockLagBetweenNodes(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeTron)
	b := fakenode.New(t, "B", config.ChainTypeTron)
	a.Set(fakenode.Behavior{Block: 1000})
	b.Set(fakenode.Behavior{Block: 990})
	opts := defaultOpts()
	opts.MaxBlockLag = 5
	m := NewManager("TRX", config.ChainTypeTron, targetsOf(a, b), opts, nil)

	m.RunOnce(context.Background())

	if st := m.Status(); !st[0].Routable || st[1].Routable || st[1].Lag != 10 {
		t.Errorf("lag check must work for tron: %+v", st)
	}
}

func TestTron_PassesPathQueryMethodAndBody(t *testing.T) {
	n := fakenode.New(t, "Grid", config.ChainTypeTron)
	p := newTronProxy(t, targetsOf(n), nil, 0)

	rr := tronRequest(p, http.MethodPost, "/wallet/triggersmartcontract", `{"owner_address":"41abc"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	e := decodeEcho(t, rr)
	if e.Path != "/wallet/triggersmartcontract" || e.HTTPMethod != http.MethodPost || e.Body != `{"owner_address":"41abc"}` {
		t.Errorf("POST not passed through: %+v", e)
	}

	rr = tronRequest(p, http.MethodGet, "/v1/accounts/TAbc/transactions?limit=5&only_confirmed=true", "")
	e = decodeEcho(t, rr)
	if e.Path != "/v1/accounts/TAbc/transactions" || e.HTTPMethod != http.MethodGet || e.Query != "limit=5&only_confirmed=true" {
		t.Errorf("GET with query not passed through: %+v", e)
	}
	if rr.Header().Get("X-Rpc-Provider") != "Grid" {
		t.Error("provider header missing")
	}
	ev := p.rec.Of(events.KindUpstreamRequest, "Grid")
	if len(ev) != 2 || ev[1].Method != "GET /v1/accounts/TAbc/transactions" {
		t.Errorf("upstream events should name the path for non JSON-RPC calls: %+v", ev)
	}
}

func TestTron_TargetBasePathAndQueryAreKept(t *testing.T) {
	n := fakenode.New(t, "Grid", config.ChainTypeTron)
	target := n.Target()
	target.HTTPURL = n.URL() + "/base/?apikey=k1"
	p := newTronProxy(t, []config.Target{target}, nil, 0)

	rr := tronRequest(p, http.MethodGet, "/v1/blocks?limit=1", "")
	e := decodeEcho(t, rr)
	if e.Path != "/base/v1/blocks" || e.Query != "apikey=k1&limit=1" {
		t.Errorf("base path/query must be merged with the client's: %+v", e)
	}
}

func TestTron_HeadersInjectedAndOverrideClient(t *testing.T) {
	n := fakenode.New(t, "Grid", config.ChainTypeTron)
	target := n.Target()
	target.Headers = map[string]string{"TRON-PRO-API-KEY": "server-key", "X-Extra": "1"}
	p := newTronProxy(t, []config.Target{target}, nil, 0)

	req := httptest.NewRequest(http.MethodPost, "/wallet/getaccount", strings.NewReader(`{}`))
	req.Header.Set("TRON-PRO-API-KEY", "client-key") // must not leak through
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	h := n.Calls()[0].Header
	if h.Get("TRON-PRO-API-KEY") != "server-key" || h.Get("X-Extra") != "1" {
		t.Errorf("headers not injected: %v", h)
	}
}

func TestTron_JSONRPCThroughSamePrefix(t *testing.T) {
	n := fakenode.New(t, "Grid", config.ChainTypeTron)
	n.Set(fakenode.Behavior{ChainID: "0x94a9059e"})
	p := newTronProxy(t, targetsOf(n), nil, 0)

	rr := tronRequest(p, http.MethodPost, "/jsonrpc", `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "0x94a9059e") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if c := n.Calls()[0]; c.Path != "/jsonrpc" || c.Method != "eth_chainId" {
		t.Errorf("jsonrpc call not passed: %+v", c)
	}
}

func TestTron_ClientSideErrorsPassThrough(t *testing.T) {
	tests := []struct {
		name string
		b    fakenode.Behavior
		want string
	}{
		{"Error body", fakenode.Behavior{TronError: "class org.tron.core.exception.ContractValidateException : Invalid address"}, "Invalid address"},
		{"result code", fakenode.Behavior{TronResultCode: "SIGERROR"}, `"code":"SIGERROR"`},
		{"http 400", fakenode.Behavior{HTTPStatus: 400}, "upstream error 400"},
		{"http 404", fakenode.Behavior{HTTPStatus: 404}, "upstream error 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeTron)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", config.ChainTypeTron)
			p := newTronProxy(t, targetsOf(bad, good), []config.Exception{{Match: "SERVER_BUSY"}}, time.Second)

			sawBad := false
			for i := 0; i < 20 && !sawBad; i++ {
				rr := tronRequest(p, http.MethodPost, "/wallet/broadcasttransaction", `{}`)
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

func TestTron_NodeSideFailuresFailOver(t *testing.T) {
	tests := []struct {
		name      string
		b         fakenode.Behavior
		wantTaint bool
	}{
		{"SERVER_BUSY result code", fakenode.Behavior{TronResultCode: "SERVER_BUSY"}, false},
		{"BLOCK_UNSOLIDIFIED result code", fakenode.Behavior{TronResultCode: "BLOCK_UNSOLIDIFIED"}, false},
		{"http 502", fakenode.Behavior{HTTPStatus: 502}, true},
		{"http 429", fakenode.Behavior{HTTPStatus: 429}, true},
		{"http 403 (bad api key)", fakenode.Behavior{HTTPStatus: 403}, true},
		{"dropped connection", fakenode.Behavior{Drop: true}, true},
	}
	exceptions := []config.Exception{{Match: "SERVER_BUSY"}, {Match: "BLOCK_UNSOLIDIFIED"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeTron)
			bad.Set(tt.b)
			good := fakenode.New(t, "Good", config.ChainTypeTron)
			p := newTronProxy(t, targetsOf(bad, good), exceptions, 10*time.Second)

			for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
				rr := tronRequest(p, http.MethodPost, "/wallet/broadcasttransaction", `{"signature":["aa"]}`)
				if rr.Code != http.StatusOK || decodeEcho(t, rr).Node != "Good" {
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
				if c.Body != `{"signature":["aa"]}` || c.Path != "/wallet/broadcasttransaction" {
					t.Errorf("replayed request corrupted: %+v", c)
				}
			}
		})
	}
}

func TestTron_UpstreamTimeoutFailsOver(t *testing.T) {
	slow := fakenode.New(t, "Slow", config.ChainTypeTron)
	slow.Set(fakenode.Behavior{Hang: true})
	good := fakenode.New(t, "Good", config.ChainTypeTron)
	rec := &events.Recorder{}
	m := NewManager("TRX", config.ChainTypeTron, targetsOf(slow, good), defaultOpts(), rec)
	px, err := New(Options{Chain: "TRX", Type: config.ChainTypeTron, Targets: targetsOf(slow, good), UpstreamTimeout: 100 * time.Millisecond, Observer: rec}, m)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 20 && slow.CallCount("") == 0; i++ {
		rr := tronRequest(px, http.MethodGet, "/wallet/getnowblock", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
	}
	if slow.CallCount("") == 0 {
		t.Fatal("Slow was never selected")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("upstream_timeout not enforced for tron")
	}
}
