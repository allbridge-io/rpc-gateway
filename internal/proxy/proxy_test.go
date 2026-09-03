package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

const rpcBody = `{"jsonrpc":"2.0","id":7,"method":"eth_getBalance","params":["0xabc","latest"]}`

type testProxy struct {
	*Proxy
	rec   *events.Recorder
	clock *fakeClock
}

type proxyParams struct {
	targets         []config.Target
	exceptions      []config.Exception
	upstreamTimeout time.Duration
	taint           time.Duration
	maxBody         int64
}

func newTestProxy(t *testing.T, p proxyParams) *testProxy {
	t.Helper()
	if p.upstreamTimeout == 0 {
		p.upstreamTimeout = 2 * time.Second
	}
	rec := &events.Recorder{}
	clock := newFakeClock()
	opts := defaultOpts()
	opts.TaintDuration = p.taint
	opts.Now = clock.Now
	m := NewManager("SPL", config.ChainTypeEVM, p.targets, opts, rec)
	px, err := New(Options{
		Chain:           "SPL",
		Targets:         p.targets,
		Exceptions:      p.exceptions,
		UpstreamTimeout: p.upstreamTimeout,
		MaxBodyBytes:    p.maxBody,
		Observer:        rec,
	}, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testProxy{Proxy: px, rec: rec, clock: clock}
}

func post(p http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/SPL", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

// answeredBy extracts the node name from a fakenode result.
func answeredBy(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Result struct {
			Node string `json:"node"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not a fakenode result: %s", rr.Body.String())
	}
	return resp.Result.Node
}

func assertRPCError(t *testing.T, rr *httptest.ResponseRecorder, status int, contains string) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status %d, want %d; body %s", rr.Code, status, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || !strings.Contains(resp.Error.Message, contains) {
		t.Fatalf("expected a JSON-RPC error mentioning %q, got %s", contains, rr.Body.String())
	}
}

func TestProxy_ForwardsBodyAndReportsProvider(t *testing.T) {
	n := fakenode.New(t, "Only", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: targetsOf(n)})

	rr := post(p, rpcBody, nil)

	if rr.Code != http.StatusOK || answeredBy(t, rr) != "Only" {
		t.Fatalf("unexpected response %d %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Rpc-Provider"); got != "Only" {
		t.Errorf("X-Rpc-Provider = %q", got)
	}
	calls := n.Calls()
	if len(calls) != 1 || calls[0].Body != rpcBody || calls[0].Method != "eth_getBalance" {
		t.Errorf("body not forwarded verbatim: %+v", calls)
	}
	if calls[0].Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers not forwarded: %v", calls[0].Header)
	}
	ev := p.rec.Of(events.KindUpstreamRequest, "Only")
	if len(ev) != 1 || ev[0].Method != "eth_getBalance" || ev[0].Status != 200 || ev[0].Err != nil {
		t.Errorf("upstream request event: %+v", ev)
	}
}

func TestProxy_FailoverOnHTTPErrors(t *testing.T) {
	tests := []struct {
		name      string
		bad       fakenode.Behavior
		wantTaint bool
		reason    string
	}{
		{"500", fakenode.Behavior{HTTPStatus: 500}, true, "server error (500)"},
		{"502", fakenode.Behavior{HTTPStatus: 502}, true, "server error (502)"},
		{"429", fakenode.Behavior{HTTPStatus: 429}, true, "rate limited"},
		{"403", fakenode.Behavior{HTTPStatus: 403}, true, "access denied (403)"},
		{"401", fakenode.Behavior{HTTPStatus: 401}, true, "access denied (401)"},
		{"413", fakenode.Behavior{HTTPStatus: 413}, false, "request entity too large"},
		{"dropped connection", fakenode.Behavior{Drop: true}, true, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := fakenode.New(t, "Bad", config.ChainTypeEVM)
			good := fakenode.New(t, "Good", config.ChainTypeEVM)
			bad.Set(tt.bad)
			p := newTestProxy(t, proxyParams{targets: targetsOf(bad, good), taint: 10 * time.Second})

			// Run enough requests that the random pick hits Bad first at least once.
			hitBad := false
			for i := 0; i < 20 && !hitBad; i++ {
				rr := post(p, rpcBody, nil)
				if rr.Code != http.StatusOK || answeredBy(t, rr) != "Good" {
					t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
				}
				hitBad = bad.CallCount("") > 0
				if hitBad && rr.Header().Get("X-Rpc-Provider") != "Good" {
					t.Errorf("provider header must name the target that answered")
				}
			}
			if !hitBad {
				t.Fatal("Bad was never selected; cannot verify failover")
			}
			re := p.rec.Of(events.KindRerouted, "Bad")
			if len(re) == 0 || !containsAny(re[0].Reason, tt.reason) {
				t.Fatalf("expected a reroute event mentioning %q, got %+v", tt.reason, re)
			}
			tainted := p.Manager().Status()[0].Tainted
			if tainted != tt.wantTaint {
				t.Errorf("tainted = %v, want %v", tainted, tt.wantTaint)
			}
		})
	}
}

func TestProxy_FailoverOnConnectionRefused(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	good := fakenode.New(t, "Good", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: []config.Target{{Name: "Dead", HTTPURL: deadURL}, good.Target()}, taint: time.Second})

	for i := 0; i < 10; i++ {
		rr := post(p, rpcBody, nil)
		if rr.Code != http.StatusOK || answeredBy(t, rr) != "Good" {
			t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	if len(p.rec.Of(events.KindRerouted, "Dead")) == 0 {
		t.Fatal("expected reroutes away from the dead target")
	}
	if !p.Manager().Status()[0].Tainted {
		t.Error("connection failures must taint the target")
	}
}

func TestProxy_FailoverOnUpstreamTimeout(t *testing.T) {
	slow := fakenode.New(t, "Slow", config.ChainTypeEVM)
	slow.Set(fakenode.Behavior{Hang: true})
	good := fakenode.New(t, "Good", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: targetsOf(slow, good), upstreamTimeout: 100 * time.Millisecond, taint: time.Second})

	start := time.Now()
	hitSlow := false
	for i := 0; i < 20 && !hitSlow; i++ {
		rr := post(p, rpcBody, nil)
		if rr.Code != http.StatusOK || answeredBy(t, rr) != "Good" {
			t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
		}
		hitSlow = slow.CallCount("") > 0
	}
	if !hitSlow {
		t.Fatal("Slow was never selected")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("failover from a hanging node took %v; upstream_timeout is not enforced", elapsed)
	}
	if re := p.rec.Of(events.KindRerouted, "Slow"); len(re) == 0 || !strings.Contains(re[0].Reason, "timeout") {
		t.Errorf("expected a timeout reroute, got %+v", re)
	}
}

func TestProxy_SlowButWithinTimeoutIsNotAFailure(t *testing.T) {
	slow := fakenode.New(t, "Slow", config.ChainTypeEVM)
	slow.Set(fakenode.Behavior{Latency: 50 * time.Millisecond})
	p := newTestProxy(t, proxyParams{targets: targetsOf(slow), upstreamTimeout: 500 * time.Millisecond})

	rr := post(p, rpcBody, nil)

	if rr.Code != http.StatusOK || answeredBy(t, rr) != "Slow" {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if len(p.rec.Of(events.KindRerouted, "")) != 0 {
		t.Error("no reroute expected")
	}
}

func TestProxy_ExceptionMatchReroutesWithoutTaint(t *testing.T) {
	bad := fakenode.New(t, "Bad", config.ChainTypeEVM)
	bad.Set(fakenode.Behavior{RPCError: "Blockhash not found"})
	good := fakenode.New(t, "Good", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{
		targets:    targetsOf(bad, good),
		exceptions: []config.Exception{{Match: "Blockhash not found", Message: "Solana: Blockhash not found"}},
		taint:      10 * time.Second,
	})

	hitBad := false
	for i := 0; i < 20 && !hitBad; i++ {
		rr := post(p, rpcBody, nil)
		if rr.Code != http.StatusOK || answeredBy(t, rr) != "Good" {
			t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
		}
		hitBad = bad.CallCount("") > 0
	}
	if !hitBad {
		t.Fatal("Bad was never selected")
	}
	re := p.rec.Of(events.KindRerouted, "Bad")
	if len(re) == 0 || !strings.Contains(re[0].Reason, "Solana: Blockhash not found") {
		t.Fatalf("reroute must carry the configured message, got %+v", re)
	}
	if p.Manager().Status()[0].Tainted {
		t.Error("an exception match is request-specific and must not taint the target")
	}
}

func TestProxy_ExceptionDetectedInGzippedResponse(t *testing.T) {
	bad := fakenode.New(t, "Bad", config.ChainTypeEVM)
	bad.Set(fakenode.Behavior{RPCError: "block range is too wide", GzipResponse: true})
	good := fakenode.New(t, "Good", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{
		targets:    targetsOf(bad, good),
		exceptions: []config.Exception{{Match: "block range is too wide"}},
	})

	hitBad := false
	for i := 0; i < 20 && !hitBad; i++ {
		rr := post(p, rpcBody, map[string]string{"Accept-Encoding": "gzip"})
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
		}
		hitBad = bad.CallCount("") > 0
	}
	if !hitBad {
		t.Fatal("Bad was never selected")
	}
	if len(p.rec.Of(events.KindRerouted, "Bad")) == 0 {
		t.Fatal("exception inside a gzipped body was not detected")
	}
}

func TestProxy_RPCErrorWithoutExceptionPassesThrough(t *testing.T) {
	n := fakenode.New(t, "Only", config.ChainTypeEVM)
	n.Set(fakenode.Behavior{RPCError: "execution reverted"})
	p := newTestProxy(t, proxyParams{targets: targetsOf(n), exceptions: []config.Exception{{Match: "something else"}}})

	rr := post(p, rpcBody, nil)

	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "execution reverted") {
		t.Fatalf("a normal JSON-RPC error belongs to the client: %d %s", rr.Code, rr.Body.String())
	}
	if len(p.rec.Of(events.KindRerouted, "")) != 0 {
		t.Error("no reroute expected")
	}
}

func TestProxy_ClientErrorStatusPassesThrough(t *testing.T) {
	n := fakenode.New(t, "Only", config.ChainTypeEVM)
	n.Set(fakenode.Behavior{HTTPStatus: 400})
	p := newTestProxy(t, proxyParams{targets: targetsOf(n)})

	rr := post(p, rpcBody, nil)

	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "upstream error 400") {
		t.Fatalf("4xx (other than 401/403/413/429) must be passed through: %d %s", rr.Code, rr.Body.String())
	}
	if len(p.rec.Of(events.KindRerouted, "")) != 0 {
		t.Error("no reroute expected")
	}
}

func TestProxy_AllTargetsFailing503(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	b := fakenode.New(t, "B", config.ChainTypeEVM)
	a.Set(fakenode.Behavior{HTTPStatus: 500})
	b.Set(fakenode.Behavior{HTTPStatus: 503})
	p := newTestProxy(t, proxyParams{targets: targetsOf(a, b)})

	rr := post(p, rpcBody, nil)

	assertRPCError(t, rr, http.StatusServiceUnavailable, "no healthy upstream for chain SPL")
	if a.CallCount("") != 1 || b.CallCount("") != 1 {
		t.Errorf("each target must be tried exactly once: A=%d B=%d", a.CallCount(""), b.CallCount(""))
	}
	nh := p.rec.Of(events.KindNoHealthy, "")
	if len(nh) != 1 || nh[0].Attempted != 2 || nh[0].Chain != "SPL" {
		t.Errorf("expected one no-healthy event with attempted=2, got %+v", nh)
	}
}

func TestProxy_NoRoutableTargets503WithoutCalls(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	targets := targetsOf(a)
	targets[0].Disabled = true
	p := newTestProxy(t, proxyParams{targets: targets})

	rr := post(p, rpcBody, nil)

	assertRPCError(t, rr, http.StatusServiceUnavailable, "no healthy upstream")
	if a.CallCount("") != 0 {
		t.Error("disabled target must not be called")
	}
}

func TestProxy_UnhealthyByChecksIsSkipped(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	b := fakenode.New(t, "B", config.ChainTypeEVM)
	a.Set(fakenode.Behavior{HTTPStatus: 500})
	p := newTestProxy(t, proxyParams{targets: targetsOf(a, b)})
	p.Manager().RunOnce(context.Background()) // marks A unhealthy (threshold 1)
	a.ResetCalls()

	for i := 0; i < 10; i++ {
		rr := post(p, rpcBody, nil)
		if rr.Code != http.StatusOK || answeredBy(t, rr) != "B" {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
	}
	if a.CallCount("") != 0 {
		t.Error("a target marked unhealthy by checks must not receive requests")
	}
}

func TestProxy_TaintedTargetSkippedUntilExpiry(t *testing.T) {
	bad := fakenode.New(t, "Bad", config.ChainTypeEVM)
	bad.Set(fakenode.Behavior{HTTPStatus: 500})
	good := fakenode.New(t, "Good", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: targetsOf(bad, good), taint: 15 * time.Second})

	for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
		post(p, rpcBody, nil)
	}
	if bad.CallCount("") == 0 {
		t.Fatal("Bad was never selected")
	}
	bad.ResetCalls()
	for i := 0; i < 20; i++ {
		post(p, rpcBody, nil)
	}
	if bad.CallCount("") != 0 {
		t.Fatalf("tainted target received %d requests", bad.CallCount(""))
	}

	p.clock.Advance(16 * time.Second)
	for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
		post(p, rpcBody, nil)
	}
	if bad.CallCount("") == 0 {
		t.Error("after the taint expires the target must be tried again")
	}
}

func TestProxy_GzipRequestDecompressedForTargetWithoutCompression(t *testing.T) {
	n := fakenode.New(t, "Plain", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: targetsOf(n)})

	rr := post(p, gzipString(t, rpcBody), map[string]string{"Content-Encoding": "gzip"})

	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	c := n.Calls()[0]
	if c.Body != rpcBody || c.Header.Get("Content-Encoding") != "" || c.Header.Get("Content-Length") != itoa(len(rpcBody)) {
		t.Errorf("target must receive plain body with corrected headers: %+v", c)
	}
}

func TestProxy_GzipRequestKeptForCompressionTarget(t *testing.T) {
	n := fakenode.New(t, "Gz", config.ChainTypeEVM)
	targets := targetsOf(n)
	targets[0].Compression = true
	p := newTestProxy(t, proxyParams{targets: targets})
	compressed := gzipString(t, rpcBody)

	rr := post(p, compressed, map[string]string{"Content-Encoding": "gzip"})

	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	c := n.Calls()[0]
	if c.Body != compressed || c.Header.Get("Content-Encoding") != "gzip" {
		t.Errorf("target with compression must receive the body untouched: %+v", c)
	}
}

func TestProxy_GzipRequestReplayedOnFailover(t *testing.T) {
	bad := fakenode.New(t, "Bad", config.ChainTypeEVM)
	bad.Set(fakenode.Behavior{HTTPStatus: 500})
	good := fakenode.New(t, "Good", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: targetsOf(bad, good)})

	for i := 0; i < 20 && bad.CallCount("") == 0; i++ {
		rr := post(p, gzipString(t, rpcBody), map[string]string{"Content-Encoding": "gzip"})
		if rr.Code != http.StatusOK || answeredBy(t, rr) != "Good" {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
	}
	for _, c := range good.Calls() {
		if c.Body != rpcBody {
			t.Fatalf("replayed body corrupted: %q", c.Body)
		}
	}
}

func TestProxy_InvalidGzipIs400(t *testing.T) {
	n := fakenode.New(t, "Plain", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: targetsOf(n)})

	rr := post(p, "definitely not gzip", map[string]string{"Content-Encoding": "gzip"})

	assertRPCError(t, rr, http.StatusBadRequest, "decompress")
	if n.CallCount("") != 0 {
		t.Error("nothing should reach the target")
	}
}

func TestProxy_BodyTooLarge413(t *testing.T) {
	n := fakenode.New(t, "Only", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: targetsOf(n), maxBody: 64})

	rr := post(p, strings.Repeat("x", 65), nil)

	assertRPCError(t, rr, http.StatusRequestEntityTooLarge, "too large")
	if n.CallCount("") != 0 {
		t.Error("nothing should reach the target")
	}
}

func TestProxy_ClientCancelStopsRetries(t *testing.T) {
	slow := fakenode.New(t, "Slow", config.ChainTypeEVM)
	slow.Set(fakenode.Behavior{Latency: 300 * time.Millisecond})
	p := newTestProxy(t, proxyParams{targets: targetsOf(slow), upstreamTimeout: 5 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/SPL", strings.NewReader(rpcBody)).WithContext(ctx)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)

	if len(p.rec.Of(events.KindRerouted, "")) != 0 || len(p.rec.Of(events.KindNoHealthy, "")) != 0 {
		t.Errorf("a client that went away must not trigger reroutes: %+v", p.rec.All())
	}
	if p.Manager().Status()[0].Tainted {
		t.Error("client cancellation is not the target's fault")
	}
}

// --- WebSocket ---

// wsEcho is a WebSocket server that echoes text messages, prefixed with its name.
func wsEcho(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, append([]byte(name+":"), data...)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsRoundTrip(t *testing.T, gatewayURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, strings.Replace(gatewayURL, "http://", "ws://", 1)+"/SPL", nil)
	if err != nil {
		t.Fatalf("ws dial through gateway: %v", err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	return string(data)
}

func TestProxy_WebSocketUpgradeProxied(t *testing.T) {
	echo := wsEcho(t, "ws1")
	http1 := fakenode.New(t, "N1", config.ChainTypeSolana)
	targets := []config.Target{{Name: "N1", HTTPURL: http1.URL(), WSURL: strings.Replace(echo.URL, "http://", "ws://", 1)}}
	p := newTestProxy(t, proxyParams{targets: targets})
	gw := httptest.NewServer(p)
	defer gw.Close()

	if got := wsRoundTrip(t, gw.URL); got != "ws1:ping" {
		t.Fatalf("echo through proxy = %q", got)
	}
	if http1.CallCount("") != 0 {
		t.Error("WebSocket traffic must use ws_url, not http_url")
	}
}

func TestProxy_WebSocketFallsBackToHTTPURL(t *testing.T) {
	echo := wsEcho(t, "same")
	targets := []config.Target{{Name: "N1", HTTPURL: echo.URL}} // no ws_url
	p := newTestProxy(t, proxyParams{targets: targets})
	gw := httptest.NewServer(p)
	defer gw.Close()

	if got := wsRoundTrip(t, gw.URL); got != "same:ping" {
		t.Fatalf("echo = %q", got)
	}
}

func TestProxy_WebSocketFailoverOnHandshakeError(t *testing.T) {
	broken := fakenode.New(t, "Broken", config.ChainTypeSolana)
	broken.Set(fakenode.Behavior{HTTPStatus: 503})
	echo := wsEcho(t, "ws2")
	targets := []config.Target{
		{Name: "Broken", HTTPURL: broken.URL(), WSURL: strings.Replace(broken.URL(), "http://", "ws://", 1)},
		{Name: "Good", HTTPURL: echo.URL, WSURL: strings.Replace(echo.URL, "http://", "ws://", 1)},
	}
	p := newTestProxy(t, proxyParams{targets: targets, taint: time.Second})
	gw := httptest.NewServer(p)
	defer gw.Close()

	for i := 0; i < 20 && broken.CallCount("") == 0; i++ {
		if got := wsRoundTrip(t, gw.URL); got != "ws2:ping" {
			t.Fatalf("echo = %q", got)
		}
	}
	if broken.CallCount("") == 0 {
		t.Fatal("Broken was never selected")
	}
	if len(p.rec.Of(events.KindRerouted, "Broken")) == 0 {
		t.Error("failed handshake must be rerouted")
	}
}

// --- helpers ---

func gzipString(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func itoa(n int) string { return strconv.Itoa(n) }
