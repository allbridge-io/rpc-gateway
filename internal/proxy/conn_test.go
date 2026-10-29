package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

// Both the proxy and the health checker must ping their HTTP/2 connections:
// without it a silently dead connection keeps every request to that provider
// hanging until its timeout, for as long as TCP takes to notice (minutes).
func TestTransports_PingHTTP2Connections(t *testing.T) {
	h2 := newHealthCheckClient().Transport.(*http.Transport).HTTP2
	if h2 == nil || h2.SendPingTimeout <= 0 || h2.PingTimeout <= 0 {
		t.Errorf("health check transport must ping HTTP/2 connections: %+v", h2)
	}

	httpProxy, wsProxy, err := newReverseProxies(config.Target{Name: "T", HTTPURL: "https://example.com"}, config.ChainTypeEVM, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*httputil.ReverseProxy{"http": httpProxy, "ws": wsProxy} {
		h2 := p.Transport.(*http.Transport).HTTP2
		if h2 == nil || h2.SendPingTimeout <= 0 || h2.PingTimeout <= 0 {
			t.Errorf("%s proxy transport must ping HTTP/2 connections: %+v", name, h2)
		}
	}
}

func TestConnTrace_Describe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: &http.Transport{}}

	get := func() string {
		ctx, c := withConnTrace(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return c.describe()
	}
	if got := get(); got != "new connection" {
		t.Errorf("first request: %q", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := get(); !strings.HasPrefix(got, "reused connection, idle ") {
		t.Errorf("second request: %q, want a reused idle connection", got)
	}

	_, c := withConnTrace(context.Background())
	if got := c.describe(); got != "no connection obtained" {
		t.Errorf("no request: %q", got)
	}
}

// A check that runs out of time says which connection it hung on, so a dead
// pooled connection can be told from a provider that does not answer.
func TestManager_TimeoutNamesTheConnection(t *testing.T) {
	n := fakenode.New(t, "Slow", config.ChainTypeEVM)
	n.Set(fakenode.Behavior{Block: 100})
	opts := defaultOpts()
	opts.Timeout = 200 * time.Millisecond
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(n), opts, nil)

	m.RunOnce(context.Background()) // healthy: leaves an idle connection in the pool
	if st := m.Status()[0]; !st.Routable {
		t.Fatalf("first round: %+v", st)
	}

	n.Set(fakenode.Behavior{Hang: true})
	m.RunOnce(context.Background())
	if got := m.Status()[0].LastError; !strings.Contains(got, "deadline exceeded (reused connection, idle ") {
		t.Errorf("timeout on a pooled connection: %q", got)
	}

	// HTTP/1.1 drops a connection whose request was cancelled, so the next
	// check dials anew.
	m.RunOnce(context.Background())
	if got := m.Status()[0].LastError; !strings.Contains(got, "deadline exceeded (new connection)") {
		t.Errorf("timeout on a fresh connection: %q", got)
	}

	// Errors that are not timeouts are about the answer, not the connection.
	n.Set(fakenode.Behavior{HTTPStatus: 500})
	m.RunOnce(context.Background())
	if got := m.Status()[0].LastError; strings.Contains(got, "connection") {
		t.Errorf("a non-timeout error must not be annotated: %q", got)
	}
}

func TestProxy_TransportFailureNamesTheConnection(t *testing.T) {
	hang := fakenode.New(t, "Hang", config.ChainTypeEVM)
	hang.Set(fakenode.Behavior{Hang: true})
	bad := fakenode.New(t, "Bad", config.ChainTypeEVM)
	bad.Set(fakenode.Behavior{HTTPStatus: 502})
	good := fakenode.New(t, "Good", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: targetsOf(hang, bad, good), upstreamTimeout: 100 * time.Millisecond, taint: time.Second})

	for i := 0; i < 50 && (hang.CallCount("") == 0 || bad.CallCount("") == 0); i++ {
		if rr := post(p, rpcBody, nil); rr.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	if re := p.rec.Of(events.KindRerouted, "Hang"); len(re) == 0 || !strings.Contains(re[0].Reason, "timeout awaiting response headers (new connection)") {
		t.Errorf("a transport timeout must name its connection: %+v", re)
	}
	// An HTTP status is the target's answer: nothing to say about the connection.
	if re := p.rec.Of(events.KindRerouted, "Bad"); len(re) == 0 || strings.Contains(re[0].Reason, "connection") {
		t.Errorf("an HTTP error must not be annotated: %+v", re)
	}
}
