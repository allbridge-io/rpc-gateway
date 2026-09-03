package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

// fakeClock lets tests move time forward without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// containsAny reports whether s contains at least one of the '|'-separated alternatives.
func containsAny(s, alternatives string) bool {
	for _, alt := range strings.Split(alternatives, "|") {
		if strings.Contains(s, alt) {
			return true
		}
	}
	return false
}

func defaultOpts() HealthOptions {
	return HealthOptions{
		Interval:         time.Hour, // never ticks in tests; RunOnce is called directly
		Timeout:          2 * time.Second,
		FailureThreshold: 1,
		SuccessThreshold: 1,
		MaxBlockLag:      0,
		TaintDuration:    0,
	}
}

func targetsOf(nodes ...*fakenode.Node) []config.Target {
	out := make([]config.Target, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Target())
	}
	return out
}

func TestManager_AllHealthyAfterRound(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	b := fakenode.New(t, "B", config.ChainTypeEVM)
	a.Set(fakenode.Behavior{Block: 100})
	b.Set(fakenode.Behavior{Block: 101})
	rec := &events.Recorder{}
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(a, b), defaultOpts(), rec)

	m.RunOnce(context.Background())

	st := m.Status()
	if !st[0].Routable || !st[1].Routable {
		t.Fatalf("both targets should be routable: %+v", st)
	}
	if st[0].BlockNumber != 100 || st[1].BlockNumber != 101 {
		t.Errorf("block numbers not recorded: %+v", st)
	}
	if st[1].Lag != 0 || st[0].Lag != 1 {
		t.Errorf("lag: A=%d B=%d", st[0].Lag, st[1].Lag)
	}
	if a.CallCount("eth_blockNumber") != 1 || b.CallCount("eth_blockNumber") != 1 {
		t.Errorf("each target must be checked exactly once per round")
	}
	if got := rec.All(); len(got) != 0 {
		t.Errorf("no events expected while everything is healthy, got %+v", got)
	}
}

func TestManager_SolanaUsesGetSlot(t *testing.T) {
	n := fakenode.New(t, "S", config.ChainTypeSolana)
	n.Set(fakenode.Behavior{Block: 4242})
	m := NewManager("SOL", config.ChainTypeSolana, targetsOf(n), defaultOpts(), nil)

	m.RunOnce(context.Background())

	if n.CallCount("getSlot") != 1 || n.CallCount("eth_blockNumber") != 0 {
		t.Errorf("solana must be checked with getSlot; calls: %+v", n.Calls())
	}
	if st := m.Status()[0]; st.BlockNumber != 4242 || !st.Routable {
		t.Errorf("unexpected status %+v", st)
	}
}

func TestManager_FailureAndSuccessThresholds(t *testing.T) {
	n := fakenode.New(t, "A", config.ChainTypeEVM)
	rec := &events.Recorder{}
	opts := defaultOpts()
	opts.FailureThreshold = 2
	opts.SuccessThreshold = 2
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(n), opts, rec)
	ctx := context.Background()

	n.Set(fakenode.Behavior{HTTPStatus: 500})
	m.RunOnce(ctx)
	if !m.IsRoutable(0) {
		t.Fatal("one failure must not exceed failure_threshold=2")
	}
	if len(rec.Of(events.KindHealthChanged, "")) != 0 {
		t.Fatal("no event expected before the threshold is reached")
	}

	m.RunOnce(ctx)
	if m.IsRoutable(0) {
		t.Fatal("two consecutive failures must mark the target unhealthy")
	}
	down := rec.Of(events.KindHealthChanged, "A")
	if len(down) != 1 || down[0].Healthy || !strings.Contains(down[0].Reason, "2 consecutive failed checks") {
		t.Fatalf("expected one unhealthy event with the reason, got %+v", down)
	}
	if st := m.Status()[0]; st.ConsecutiveFailures != 2 || !strings.Contains(st.LastError, "500") {
		t.Errorf("status must expose failures and last error: %+v", st)
	}

	m.RunOnce(ctx) // third failure: still unhealthy, no duplicate event
	if len(rec.Of(events.KindHealthChanged, "A")) != 1 {
		t.Fatal("state did not change; no new event expected")
	}

	n.Set(fakenode.Behavior{Block: 10})
	m.RunOnce(ctx)
	if m.IsRoutable(0) {
		t.Fatal("one success must not exceed success_threshold=2")
	}
	m.RunOnce(ctx)
	if !m.IsRoutable(0) {
		t.Fatal("two consecutive successes must mark the target healthy")
	}
	up := rec.Of(events.KindHealthChanged, "A")
	if len(up) != 2 || !up[1].Healthy || !strings.Contains(up[1].Reason, "recovered") {
		t.Fatalf("expected a recovery event, got %+v", up)
	}
}

func TestManager_CheckFailureKinds(t *testing.T) {
	tests := []struct {
		name    string
		b       fakenode.Behavior
		wantErr string
	}{
		{"http 500", fakenode.Behavior{HTTPStatus: 500}, "http status 500"},
		{"http 429", fakenode.Behavior{HTTPStatus: 429}, "http status 429"},
		{"http 403", fakenode.Behavior{HTTPStatus: 403}, "http status 403"},
		{"invalid json", fakenode.Behavior{RawBody: "<html>nope</html>"}, "invalid json-rpc response"},
		{"json-rpc error", fakenode.Behavior{RPCError: "internal boom"}, "internal boom"},
		{"null result", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":null}`}, "empty json-rpc result"},
		{"wrong result type", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":{"x":1}}`}, "unexpected result"},
		{"not hex", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":"0xzz"}`}, "invalid hex"},
		{"hex without prefix", fakenode.Behavior{RawBody: `{"jsonrpc":"2.0","id":1,"result":"12345"}`}, "missing 0x prefix"},
		{"connection dropped", fakenode.Behavior{Drop: true}, "connection reset|EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := fakenode.New(t, "A", config.ChainTypeEVM)
			n.Set(tt.b)
			m := NewManager("SPL", config.ChainTypeEVM, targetsOf(n), defaultOpts(), nil)

			m.RunOnce(context.Background())

			st := m.Status()[0]
			if st.Routable {
				t.Fatalf("target must be unhealthy after %s", tt.name)
			}
			if !containsAny(st.LastError, tt.wantErr) {
				t.Errorf("last error %q does not mention %q", st.LastError, tt.wantErr)
			}
		})
	}
}

func TestManager_TimeoutIsAFailureAndBounded(t *testing.T) {
	n := fakenode.New(t, "Slow", config.ChainTypeEVM)
	n.Set(fakenode.Behavior{Hang: true})
	opts := defaultOpts()
	opts.Timeout = 100 * time.Millisecond
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(n), opts, nil)

	start := time.Now()
	m.RunOnce(context.Background())
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("a hanging node must not block the round longer than the timeout, took %v", elapsed)
	}
	if st := m.Status()[0]; st.Routable || !strings.Contains(st.LastError, "deadline exceeded") {
		t.Errorf("hanging node must be unhealthy with a timeout error: %+v", st)
	}
}

func TestManager_ConnectionRefusedIsAFailure(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	m := NewManager("SPL", config.ChainTypeEVM, []config.Target{{Name: "Dead", HTTPURL: url}}, defaultOpts(), nil)

	m.RunOnce(context.Background())

	if st := m.Status()[0]; st.Routable || st.LastError == "" {
		t.Errorf("unreachable node must be unhealthy: %+v", st)
	}
}

func TestManager_BlockLag(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	b := fakenode.New(t, "B", config.ChainTypeEVM)
	rec := &events.Recorder{}
	opts := defaultOpts()
	opts.MaxBlockLag = 20
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(a, b), opts, rec)
	ctx := context.Background()

	a.Set(fakenode.Behavior{Block: 1000})
	b.Set(fakenode.Behavior{Block: 950})
	m.RunOnce(ctx)
	st := m.Status()
	if !st[0].Routable || st[1].Routable || !st[1].Lagging || st[1].Lag != 50 {
		t.Fatalf("B is 50 blocks behind and must be excluded: %+v", st)
	}
	if !st[1].CheckHealthy {
		t.Error("lag is not a check failure; checkHealthy must stay true")
	}
	ev := rec.Of(events.KindHealthChanged, "B")
	if len(ev) != 1 || ev[0].Healthy || !strings.Contains(ev[0].Reason, "lagging 50 blocks") {
		t.Fatalf("expected a lagging event, got %+v", ev)
	}

	b.Set(fakenode.Behavior{Block: 1000 - 20}) // exactly at the limit is fine
	m.RunOnce(ctx)
	if st := m.Status(); !st[1].Routable {
		t.Fatalf("lag equal to the limit must be tolerated: %+v", st)
	}
	ev = rec.Of(events.KindHealthChanged, "B")
	if len(ev) != 2 || !ev[1].Healthy || !strings.Contains(ev[1].Reason, "caught up") {
		t.Fatalf("expected a caught-up event, got %+v", ev)
	}
}

func TestManager_LagDisabled(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	b := fakenode.New(t, "B", config.ChainTypeEVM)
	a.Set(fakenode.Behavior{Block: 1000})
	b.Set(fakenode.Behavior{Block: 1})
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(a, b), defaultOpts(), nil) // MaxBlockLag 0

	m.RunOnce(context.Background())

	if st := m.Status(); !st[1].Routable || st[1].Lag != 999 {
		t.Errorf("with lag check disabled B stays routable but lag is still reported: %+v", st)
	}
}

func TestManager_LagNotJudgedWhenCheckFails(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	b := fakenode.New(t, "B", config.ChainTypeEVM)
	a.Set(fakenode.Behavior{Block: 1000})
	b.Set(fakenode.Behavior{HTTPStatus: 502})
	opts := defaultOpts()
	opts.MaxBlockLag = 5
	rec := &events.Recorder{}
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(a, b), opts, rec)

	m.RunOnce(context.Background())

	st := m.Status()[1]
	if st.Lagging || st.Routable || st.CheckHealthy {
		t.Errorf("a failing node is unhealthy by checks, not by lag: %+v", st)
	}
	if ev := rec.Of(events.KindHealthChanged, "B"); len(ev) != 1 || !strings.Contains(ev[0].Reason, "failed checks") {
		t.Errorf("exactly one unhealthy event (from checks) expected, got %+v", ev)
	}
}

func TestManager_Taint(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	clock := newFakeClock()
	rec := &events.Recorder{}
	opts := defaultOpts()
	opts.TaintDuration = 15 * time.Second
	opts.Now = clock.Now
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(a), opts, rec)
	m.RunOnce(context.Background())

	m.Taint(0, "server error (500)")
	if m.IsRoutable(0) {
		t.Fatal("tainted target must not be routable")
	}
	if st := m.Status()[0]; !st.Tainted || st.TaintReason != "server error (500)" || !st.CheckHealthy {
		t.Errorf("status must show the taint but keep check health: %+v", st)
	}
	if ev := rec.Of(events.KindTainted, "A"); len(ev) != 1 || ev[0].Duration != 15*time.Second {
		t.Fatalf("expected one taint event, got %+v", ev)
	}

	m.Taint(0, "again") // re-taint while tainted: extends silently
	if ev := rec.Of(events.KindTainted, "A"); len(ev) != 1 {
		t.Fatal("re-tainting an already tainted target must not spam events")
	}

	clock.Advance(14 * time.Second)
	if m.IsRoutable(0) {
		t.Fatal("still within the taint window")
	}
	clock.Advance(2 * time.Second)
	if !m.IsRoutable(0) {
		t.Fatal("taint must expire after the duration")
	}
	if st := m.Status()[0]; st.Tainted || st.TaintReason != "" {
		t.Errorf("expired taint must not show in status: %+v", st)
	}
}

func TestManager_TaintDisabled(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	rec := &events.Recorder{}
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(a), defaultOpts(), rec) // TaintDuration 0

	m.Taint(0, "whatever")

	if !m.IsRoutable(0) || len(rec.All()) != 0 {
		t.Error("taint_duration = 0 must make Taint a no-op")
	}
}

func TestManager_DisabledTargetIsNeitherCheckedNorRouted(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	b := fakenode.New(t, "B", config.ChainTypeEVM)
	targets := targetsOf(a, b)
	targets[0].Disabled = true
	m := NewManager("SPL", config.ChainTypeEVM, targets, defaultOpts(), nil)

	m.RunOnce(context.Background())

	if a.CallCount("") != 0 {
		t.Error("disabled target must not be health-checked")
	}
	if st := m.Status()[0]; !st.Disabled || st.Routable {
		t.Errorf("disabled target must be reported and not routable: %+v", st)
	}
	for i := 0; i < 50; i++ {
		if idx := m.NextRoutable(nil); idx != 1 {
			t.Fatalf("NextRoutable returned %d, want 1 (the only enabled target)", idx)
		}
	}
}

func TestManager_NextRoutable(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	b := fakenode.New(t, "B", config.ChainTypeEVM)
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(a, b), defaultOpts(), nil)
	m.RunOnce(context.Background())

	seen := map[int]int{}
	for i := 0; i < 200; i++ {
		seen[m.NextRoutable(nil)]++
	}
	if seen[0] < 40 || seen[1] < 40 {
		t.Errorf("selection must spread over both targets, got %v", seen)
	}
	for i := 0; i < 50; i++ {
		if idx := m.NextRoutable([]int{0}); idx != 1 {
			t.Fatalf("excluding 0 must always yield 1, got %d", idx)
		}
	}
	if idx := m.NextRoutable([]int{0, 1}); idx != -1 {
		t.Errorf("all excluded must yield -1, got %d", idx)
	}

	b.Set(fakenode.Behavior{HTTPStatus: 503})
	m.RunOnce(context.Background())
	for i := 0; i < 50; i++ {
		if idx := m.NextRoutable(nil); idx != 0 {
			t.Fatalf("unhealthy B must never be picked, got %d", idx)
		}
	}
	a.Set(fakenode.Behavior{HTTPStatus: 503})
	m.RunOnce(context.Background())
	if idx := m.NextRoutable(nil); idx != -1 {
		t.Errorf("no healthy targets must yield -1, got %d", idx)
	}
}

func TestManager_EmptyTargets(t *testing.T) {
	m := NewManager("SPL", config.ChainTypeEVM, nil, defaultOpts(), nil)
	m.RunOnce(context.Background())
	if m.NextRoutable(nil) != -1 || len(m.Status()) != 0 {
		t.Error("empty manager must be harmless")
	}
}

func TestManager_StartStopsWithContext(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	opts := defaultOpts()
	opts.Interval = 20 * time.Millisecond
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(a), opts, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Start(ctx)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for a.CallCount("eth_blockNumber") < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if a.CallCount("eth_blockNumber") < 3 {
		t.Fatal("periodic checks did not run")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start must return once the context is cancelled")
	}
}

// Run with -race: concurrent rounds, taints and reads must not race.
func TestManager_ConcurrentUse(t *testing.T) {
	a := fakenode.New(t, "A", config.ChainTypeEVM)
	b := fakenode.New(t, "B", config.ChainTypeEVM)
	opts := defaultOpts()
	opts.TaintDuration = time.Millisecond
	opts.MaxBlockLag = 1
	m := NewManager("SPL", config.ChainTypeEVM, targetsOf(a, b), opts, &events.Recorder{})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				m.RunOnce(context.Background())
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				m.NextRoutable(nil)
				m.Status()
				m.Taint(j%2, "x")
			}
		}()
	}
	wg.Wait()
}
