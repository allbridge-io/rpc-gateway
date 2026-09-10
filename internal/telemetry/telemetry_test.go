package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/proto"

	"github.com/0xProject/rpc-gateway/internal/events"
)

// collector is a fake OTLP/HTTP endpoint that decodes what the SDK sends.
type collector struct {
	srv     *httptest.Server
	mu      sync.Mutex
	logs    []*collectorlogs.ExportLogsServiceRequest
	metrics []*collectormetrics.ExportMetricsServiceRequest
	status  int
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{status: http.StatusOK}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.status != http.StatusOK {
			w.WriteHeader(c.status)
			return
		}
		switch r.URL.Path {
		case "/v1/logs":
			var req collectorlogs.ExportLogsServiceRequest
			if err := proto.Unmarshal(body, &req); err != nil {
				t.Errorf("decode logs: %v", err)
			}
			c.logs = append(c.logs, &req)
		case "/v1/metrics":
			var req collectormetrics.ExportMetricsServiceRequest
			if err := proto.Unmarshal(body, &req); err != nil {
				t.Errorf("decode metrics: %v", err)
			}
			c.metrics = append(c.metrics, &req)
		default:
			t.Errorf("unexpected OTLP path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) logRecords() []logRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []logRecord
	for _, req := range c.logs {
		for _, rl := range req.ResourceLogs {
			var service string
			for _, a := range rl.Resource.GetAttributes() {
				if a.Key == "service.name" {
					service = a.Value.GetStringValue()
				}
			}
			for _, sl := range rl.ScopeLogs {
				for _, r := range sl.LogRecords {
					rec := logRecord{service: service, body: r.Body.GetStringValue(), severity: r.SeverityText, attrs: map[string]string{}}
					for _, a := range r.Attributes {
						rec.attrs[a.Key] = a.Value.GetStringValue()
					}
					out = append(out, rec)
				}
			}
		}
	}
	return out
}

type logRecord struct {
	service  string
	body     string
	severity string
	attrs    map[string]string
}

// metric returns the data points of a metric by name, keyed by their attributes.
func (c *collector) metric(name string) map[string]*metricspb.NumberDataPoint {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]*metricspb.NumberDataPoint{}
	for _, req := range c.metrics {
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name != name {
						continue
					}
					var points []*metricspb.NumberDataPoint
					if s := m.GetSum(); s != nil {
						points = s.DataPoints
					} else if g := m.GetGauge(); g != nil {
						points = g.DataPoints
					}
					for _, p := range points {
						out[attrKey(p.Attributes)] = p
					}
				}
			}
		}
	}
	return out
}

func (c *collector) histogramCount(name string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var count uint64
	for _, req := range c.metrics {
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if m.Name == name && m.GetHistogram() != nil {
						count = 0 // cumulative temporality: the latest export carries the totals
						for _, p := range m.GetHistogram().DataPoints {
							count += p.Count
						}
					}
				}
			}
		}
	}
	return count
}

// attrKey renders attributes as "k=v;k=v;" in the order the SDK emits them (sorted by key).
func attrKey(attrs []*commonpb.KeyValue) string {
	key := ""
	for _, a := range attrs {
		key += a.Key + "=" + anyValueString(a.Value) + ";"
	}
	return key
}

func anyValueString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		if x.BoolValue {
			return "true"
		}
		return "false"
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprint(x.IntValue)
	default:
		return v.String()
	}
}

func setupWithCollector(t *testing.T) (*Telemetry, *collector, *observer.ObservedLogs) {
	t.Helper()
	c := newCollector(t)
	t.Setenv(EnvEndpoint, c.srv.URL)
	t.Setenv("OTEL_SERVICE_NAME", "gateway-test")
	errCore, errLogs := observer.New(zapcore.DebugLevel)
	tel, err := Setup(context.Background(), Options{Version: "v-test", ErrorLog: zap.New(errCore)})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(ctx)
	})
	return tel, c, errLogs
}

func TestEnabled(t *testing.T) {
	t.Setenv(EnvEndpoint, "")
	if Enabled() {
		t.Error("must be disabled without an endpoint")
	}
	t.Setenv(EnvEndpoint, "https://otlp-gateway-prod-eu-west-2.grafana.net/otlp")
	if !Enabled() {
		t.Error("must be enabled with an endpoint")
	}
}

func TestLogsReachTheCollectorWithEventFields(t *testing.T) {
	tel, c, _ := setupWithCollector(t)
	log := zap.New(mustCore(t, tel, zapcore.InfoLevel))
	obs := events.NewLogger(log)

	obs.TargetHealthChanged("SOL", "Helius", false, "3 consecutive failed checks")
	obs.RequestRerouted("SPL", "Alchemy", "rate limited (429)")
	obs.UpstreamRequest("SPL", "Alchemy", "eth_call", 200, time.Millisecond, nil) // debug: filtered out
	log.Debug("noise")

	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	records := c.logRecords()
	if len(records) != 2 {
		t.Fatalf("expected exactly the two info+ records, got %+v", records)
	}
	unhealthy := records[0]
	if unhealthy.body != "target unhealthy" || unhealthy.severity != "warn" || unhealthy.service != "gateway-test" {
		t.Errorf("record: %+v", unhealthy)
	}
	if unhealthy.attrs["chain"] != "SOL" || unhealthy.attrs["target"] != "Helius" || unhealthy.attrs["reason"] != "3 consecutive failed checks" {
		t.Errorf("event fields must become log attributes: %v", unhealthy.attrs)
	}
	if records[1].body != "request rerouted" || records[1].attrs["reason"] != "rate limited (429)" {
		t.Errorf("record: %+v", records[1])
	}
}

// The application logger is built with zap.AddCaller() so stdout lines name the
// call site; the OTLP core must not turn that into code.file.path & co.
func TestOTLPRecordsCarryNoCallerAttributes(t *testing.T) {
	tel, c, _ := setupWithCollector(t)
	log := zap.New(mustCore(t, tel, zapcore.InfoLevel), zap.AddCaller())

	log.Warn("target unhealthy", zap.String("chain", "SOL"))

	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	records := c.logRecords()
	if len(records) != 1 {
		t.Fatalf("expected one record, got %+v", records)
	}
	attrs := records[0].attrs
	if attrs["chain"] != "SOL" {
		t.Errorf("event fields must survive: %v", attrs)
	}
	for k := range attrs {
		if strings.HasPrefix(k, "code.") {
			t.Errorf("caller attribute %q must not be exported: %v", k, attrs)
		}
	}
}

func TestMetricsReachTheCollector(t *testing.T) {
	tel, c, _ := setupWithCollector(t)
	snapshot := []TargetSnapshot{
		{Chain: "SPL", Target: "Alchemy", Routable: true, BlockNumber: 100, Lag: 0},
		{Chain: "SPL", Target: "Dead", Routable: false, BlockNumber: 90, Lag: 10},
		{Chain: "SPL", Target: "Off", Routable: false, Disabled: true, BlockNumber: 0, Lag: 0},
	}
	m, err := tel.Metrics(func() []TargetSnapshot { return snapshot })
	if err != nil {
		t.Fatal(err)
	}

	m.UpstreamRequest("SPL", "Alchemy", "eth_call", 200, 20*time.Millisecond, nil)
	m.UpstreamRequest("SPL", "Alchemy", "eth_call", 200, 30*time.Millisecond, nil)
	m.UpstreamRequest("SPL", "Dead", "eth_call", 0, time.Second, errors.New("connection refused"))
	m.UpstreamRequest("TRX", "TronGrid", "GET /v1/accounts/TAbc/transactions", 200, time.Millisecond, nil)
	m.RequestRerouted("SPL", "Dead", "connection refused")
	m.TargetTainted("SPL", "Dead", "connection refused", 15*time.Second)
	m.TargetHealthChanged("SPL", "Dead", false, "x")
	m.NoHealthyTargets("SOL", 2)

	if err := tel.ForceFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	req := c.metric("rpc_gateway.upstream.requests")
	if p := req["chain=SPL;method=eth_call;outcome=ok;target=Alchemy;"]; p == nil || p.GetAsInt() != 2 {
		t.Errorf("requests ok: %+v", req)
	}
	if p := req["chain=SPL;method=eth_call;outcome=error;target=Dead;"]; p == nil || p.GetAsInt() != 1 {
		t.Errorf("requests error: %+v", req)
	}
	if p := req["chain=TRX;method=GET /v1/accounts;outcome=ok;target=TronGrid;"]; p == nil {
		t.Errorf("tron path must be normalised to two segments, got keys: %v", keys(req))
	}
	if c.histogramCount("rpc_gateway.upstream.duration") != 4 {
		t.Errorf("duration histogram count = %d", c.histogramCount("rpc_gateway.upstream.duration"))
	}
	if p := c.metric("rpc_gateway.reroutes")["chain=SPL;target=Dead;"]; p == nil || p.GetAsInt() != 1 {
		t.Error("reroutes counter missing")
	}
	if p := c.metric("rpc_gateway.target.taints")["chain=SPL;target=Dead;"]; p == nil || p.GetAsInt() != 1 {
		t.Error("taints counter missing")
	}
	if p := c.metric("rpc_gateway.target.health_changes")["chain=SPL;healthy=false;target=Dead;"]; p == nil || p.GetAsInt() != 1 {
		t.Errorf("health_changes: %v", keys(c.metric("rpc_gateway.target.health_changes")))
	}
	if p := c.metric("rpc_gateway.no_healthy_targets")["chain=SOL;"]; p == nil || p.GetAsInt() != 1 {
		t.Error("no_healthy_targets counter missing")
	}
	routable := c.metric("rpc_gateway.target.routable")
	if routable["chain=SPL;target=Alchemy;"].GetAsInt() != 1 || routable["chain=SPL;target=Dead;"].GetAsInt() != 0 {
		t.Errorf("routable gauge: %v", keys(routable))
	}
	if routable["chain=SPL;target=Off;"].GetAsInt() != 0 {
		t.Errorf("a disabled target is never routable: %v", keys(routable))
	}
	disabled := c.metric("rpc_gateway.target.disabled")
	if disabled["chain=SPL;target=Off;"].GetAsInt() != 1 {
		t.Errorf("disabled gauge: %v", keys(disabled))
	}
	if disabled["chain=SPL;target=Alchemy;"].GetAsInt() != 0 || disabled["chain=SPL;target=Dead;"].GetAsInt() != 0 {
		t.Errorf("only the config-disabled target may report 1: %v", keys(disabled))
	}
	if c.metric("rpc_gateway.target.block_number")["chain=SPL;target=Dead;"].GetAsInt() != 90 ||
		c.metric("rpc_gateway.target.lag")["chain=SPL;target=Dead;"].GetAsInt() != 10 {
		t.Error("block/lag gauges wrong")
	}
}

func TestGaugesWaitForSnapshot(t *testing.T) {
	tel, c, _ := setupWithCollector(t)
	m, err := tel.Metrics(nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = tel.ForceFlush(context.Background())
	if len(c.metric("rpc_gateway.target.routable")) != 0 {
		t.Error("no gauges expected before a snapshot source is set")
	}
	m.SetSnapshot(func() []TargetSnapshot { return []TargetSnapshot{{Chain: "SOL", Target: "Public", Routable: true}} })
	_ = tel.ForceFlush(context.Background())
	if len(c.metric("rpc_gateway.target.routable")) != 1 {
		t.Error("gauge expected after SetSnapshot")
	}
}

func TestExportFailureIsReportedNotFatal(t *testing.T) {
	tel, c, errLogs := setupWithCollector(t)
	c.mu.Lock()
	c.status = http.StatusUnauthorized // wrong Grafana token
	c.mu.Unlock()
	log := zap.New(mustCore(t, tel, zapcore.InfoLevel))
	log.Warn("something")

	err := tel.ForceFlush(context.Background())
	if err == nil {
		t.Fatal("flush against a 401 endpoint should return an error")
	}
	if errLogs.FilterMessage("export error").Len() == 0 && err == nil {
		t.Error("export failures must be reported to the error log")
	}
	// The process is still fine: logging keeps working.
	log.Warn("still alive")
}

func TestNormalizeMethod(t *testing.T) {
	tests := map[string]string{
		"eth_call":                 "eth_call",
		"":                         "unknown",
		"POST /wallet/getnowblock": "POST /wallet/getnowblock",
		"GET /v1/accounts/TAbc123/transactions?x=1": "GET /v1/accounts",
		"POST /":       "POST /",
		"GET /jsonrpc": "GET /jsonrpc",
	}
	for in, want := range tests {
		if got := NormalizeMethod(in); got != want {
			t.Errorf("NormalizeMethod(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustCore(t *testing.T, tel *Telemetry, level zapcore.Level) zapcore.Core {
	t.Helper()
	core, err := tel.ZapCore(level)
	if err != nil {
		t.Fatal(err)
	}
	return core
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
