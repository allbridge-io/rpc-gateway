package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

const (
	secretPath  = "SECRET-PATH-KEY-123"
	secretQuery = "SECRET-QUERY-KEY-456"
)

func keyedDeadTarget(name string) config.Target {
	return config.Target{
		Name:    name,
		HTTPURL: "http://127.0.0.1:9/v2/" + secretPath + "?api-key=" + secretQuery,
		WSURL:   "ws://127.0.0.1:9/ws/" + secretPath + "?api-key=" + secretQuery,
	}
}

func assertNoSecret(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, secretPath) || strings.Contains(s, secretQuery) {
		t.Errorf("%s leaks a key: %q", what, s)
	}
}

func TestRedactor_ReplacesEveryFormOfTheTargetURL(t *testing.T) {
	r := newRedactor([]config.Target{keyedDeadTarget("Alchemy"), {Name: "Plain", HTTPURL: "https://plain.example/"}})
	cases := map[string]string{
		"Post \"http://127.0.0.1:9/v2/" + secretPath + "?api-key=" + secretQuery + "\": dial tcp: refused": "Post \"<Alchemy>\": dial tcp: refused",
		"ws dial http://127.0.0.1:9/ws/" + secretPath + "?api-key=" + secretQuery + " failed":              "ws dial <Alchemy> failed",
		"body echoed https://plain.example and https://plain.example/ twice":                               "body echoed <Plain> and <Plain> twice",
		"nothing to redact": "nothing to redact",
	}
	for in, want := range cases {
		if got := r.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
	if r.Error(nil) != nil {
		t.Error("Error(nil) must be nil")
	}
	plain := errors.New("plain")
	if r.Error(plain) != plain { //nolint:errorlint // identity on purpose: no wrapping when nothing was redacted
		t.Error("an error without secrets must be returned unchanged")
	}
	if r.Error(context.Canceled) != context.Canceled { //nolint:errorlint // identity on purpose
		t.Error("context.Canceled has no URL and must pass through unchanged")
	}
	withURL := r.Error(errors.New("Post \"http://127.0.0.1:9/v2/" + secretPath + "\": " + context.DeadlineExceeded.Error()))
	assertNoSecret(t, "redacted error", withURL.Error())
}

func TestRedactor_KeepsErrorChain(t *testing.T) {
	r := newRedactor([]config.Target{keyedDeadTarget("A")})
	cause := &upstreamError{reason: "server error (500) from http://127.0.0.1:9/v2/" + secretPath, taint: true, status: 500}
	err := r.Error(cause)
	assertNoSecret(t, "error text", err.Error())
	var ue *upstreamError
	if !errors.As(err, &ue) || !ue.taint {
		t.Error("errors.As must still reach the original upstreamError")
	}
}

func TestManager_StatusAndEventsNeverLeakTargetURLs(t *testing.T) {
	rec := &events.Recorder{}
	opts := defaultOpts()
	opts.Timeout = time.Second
	opts.TaintDuration = time.Minute
	m := NewManager("SPL", config.ChainTypeEVM, []config.Target{keyedDeadTarget("Alchemy")}, opts, rec)

	m.RunOnce(context.Background())
	m.Taint(0, "Post \"http://127.0.0.1:9/v2/"+secretPath+"?api-key="+secretQuery+"\": refused")

	st := m.Status()[0]
	if st.Routable || st.LastError == "" {
		t.Fatalf("dead target must be unhealthy with an error: %+v", st)
	}
	assertNoSecret(t, "status.lastError", st.LastError)
	assertNoSecret(t, "status.taintReason", st.TaintReason)
	if !strings.Contains(st.LastError, "127.0.0.1:9") {
		t.Errorf("host should survive redaction for diagnosis: %q", st.LastError)
	}
	for _, ev := range rec.All() {
		assertNoSecret(t, "event "+ev.Kind+" reason", ev.Reason)
	}
}

func TestManager_ProviderEchoingItsURLIsRedacted(t *testing.T) {
	// A provider that echoes the request URL (key included) in an error body.
	n := fakenode.New(t, "Echo", config.ChainTypeEVM)
	target := config.Target{Name: "Echo", HTTPURL: n.URL() + "/v2/" + secretPath}
	n.Set(fakenode.Behavior{HTTPStatus: http.StatusServiceUnavailable, RawBody: "upstream " + target.HTTPURL + " unavailable"})
	rec := &events.Recorder{}
	m := NewManager("SPL", config.ChainTypeEVM, []config.Target{target}, defaultOpts(), rec)

	m.RunOnce(context.Background())

	st := m.Status()[0]
	if !strings.Contains(st.LastError, "<Echo>") {
		t.Errorf("echoed URL must be replaced by the target name: %q", st.LastError)
	}
	assertNoSecret(t, "status.lastError", st.LastError)
	for _, ev := range rec.All() {
		assertNoSecret(t, "event reason", ev.Reason)
	}
}

func TestProxy_RerouteEventsNeverLeakTargetURLs(t *testing.T) {
	good := fakenode.New(t, "Good", config.ChainTypeEVM)
	p := newTestProxy(t, proxyParams{targets: []config.Target{keyedDeadTarget("Dead"), good.Target()}, taint: time.Minute})

	for i := 0; i < 20 && len(p.rec.Of(events.KindRerouted, "Dead")) == 0; i++ {
		post(p, rpcBody, nil)
	}
	if len(p.rec.Of(events.KindRerouted, "Dead")) == 0 {
		t.Fatal("Dead was never selected")
	}
	for _, ev := range p.rec.All() {
		assertNoSecret(t, "event "+ev.Kind+" reason", ev.Reason)
		if ev.Err != nil {
			assertNoSecret(t, "event "+ev.Kind+" err", ev.Err.Error())
		}
	}
	for _, s := range p.Manager().Status() {
		assertNoSecret(t, "status", s.LastError+" "+s.TaintReason)
	}
}
