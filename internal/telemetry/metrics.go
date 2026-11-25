package telemetry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/0xProject/rpc-gateway/internal/events"
)

// TargetSnapshot is what the gauges report for one target. The gateway's
// /status output maps onto it; the closure lives in main to avoid coupling.
type TargetSnapshot struct {
	Chain    string
	Target   string
	Routable bool
	// Disabled is set in the config; such a target is never routable.
	Disabled    bool
	BlockNumber uint64
	Lag         uint64
}

// SnapshotFunc returns the current state of every target of every chain.
type SnapshotFunc func() []TargetSnapshot

// Metrics is an events.Observer that records OTel metrics.
//
// Instruments (all with attributes chain and target where they apply):
//
//	rpc_gateway.upstream.requests   counter    + method, outcome (ok|error)
//	rpc_gateway.upstream.duration   histogram  seconds, + outcome (no method: see UpstreamRequest)
//	rpc_gateway.reroutes            counter    a request failed on target and was retried elsewhere
//	rpc_gateway.no_healthy_targets  counter    per chain
//	rpc_gateway.target.taints       counter
//	rpc_gateway.target.health_changes counter  + healthy (true|false)
//	rpc_gateway.target.routable     gauge      1 or 0
//	rpc_gateway.target.disabled     gauge      1 when disabled in the config
//	rpc_gateway.target.block_number gauge
//	rpc_gateway.target.lag          gauge      blocks behind the best target
type Metrics struct {
	requests      metric.Int64Counter
	duration      metric.Float64Histogram
	reroutes      metric.Int64Counter
	noHealthy     metric.Int64Counter
	taints        metric.Int64Counter
	healthChanges metric.Int64Counter

	mu       sync.RWMutex
	snapshot SnapshotFunc
}

var _ events.Observer = (*Metrics)(nil)

// Metrics creates the instruments on t's meter. snapshot feeds the gauges and
// may be nil until SetSnapshot is called (the gateway is built after the
// observer that it needs).
func (t *Telemetry) Metrics(snapshot SnapshotFunc) (*Metrics, error) {
	return NewMetrics(t.metrics.Meter("rpc-gateway"), snapshot)
}

// NewMetrics builds the instruments on any meter (tests use a manual reader).
func NewMetrics(meter metric.Meter, snapshot SnapshotFunc) (*Metrics, error) {
	m := &Metrics{snapshot: snapshot}
	var err error
	if m.requests, err = meter.Int64Counter("rpc_gateway.upstream.requests",
		metric.WithDescription("Attempts to forward a request to a target")); err != nil {
		return nil, err
	}
	if m.duration, err = meter.Float64Histogram("rpc_gateway.upstream.duration",
		metric.WithDescription("Duration of one attempt to a target"), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10)); err != nil {
		return nil, err
	}
	if m.reroutes, err = meter.Int64Counter("rpc_gateway.reroutes",
		metric.WithDescription("Requests that failed on a target and were retried on another")); err != nil {
		return nil, err
	}
	if m.noHealthy, err = meter.Int64Counter("rpc_gateway.no_healthy_targets",
		metric.WithDescription("Requests that could not be served because no target was routable")); err != nil {
		return nil, err
	}
	if m.taints, err = meter.Int64Counter("rpc_gateway.target.taints",
		metric.WithDescription("Times a target was temporarily excluded after a failed request")); err != nil {
		return nil, err
	}
	if m.healthChanges, err = meter.Int64Counter("rpc_gateway.target.health_changes",
		metric.WithDescription("Health check verdict changes")); err != nil {
		return nil, err
	}

	routable, err := meter.Int64ObservableGauge("rpc_gateway.target.routable",
		metric.WithDescription("1 when the target may receive requests"))
	if err != nil {
		return nil, err
	}
	block, err := meter.Int64ObservableGauge("rpc_gateway.target.block_number",
		metric.WithDescription("Latest block/slot seen by the health check"))
	if err != nil {
		return nil, err
	}
	lag, err := meter.Int64ObservableGauge("rpc_gateway.target.lag",
		metric.WithDescription("Blocks behind the best target of the chain"))
	if err != nil {
		return nil, err
	}
	disabled, err := meter.Int64ObservableGauge("rpc_gateway.target.disabled",
		metric.WithDescription("1 when the target is disabled in the config and therefore never routable"))
	if err != nil {
		return nil, err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		m.mu.RLock()
		fn := m.snapshot
		m.mu.RUnlock()
		if fn == nil {
			return nil
		}
		for _, s := range fn() {
			attrs := metric.WithAttributes(attribute.String("chain", s.Chain), attribute.String("target", s.Target))
			var r int64
			if s.Routable {
				r = 1
			}
			o.ObserveInt64(routable, r, attrs)
			var d int64
			if s.Disabled {
				d = 1
			}
			o.ObserveInt64(disabled, d, attrs)
			o.ObserveInt64(block, clampInt64(s.BlockNumber), attrs)
			o.ObserveInt64(lag, clampInt64(s.Lag), attrs)
		}
		return nil
	}, routable, block, lag, disabled)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// SetSnapshot installs (or replaces) the gauge data source.
func (m *Metrics) SetSnapshot(fn SnapshotFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot = fn
}

func (m *Metrics) TargetHealthChanged(chain, target string, healthy bool, _ string) {
	m.healthChanges.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("chain", chain), attribute.String("target", target), attribute.Bool("healthy", healthy)))
}

func (m *Metrics) TargetTainted(chain, target, _ string, _ time.Duration) {
	m.taints.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("chain", chain), attribute.String("target", target)))
}

func (m *Metrics) RequestRerouted(chain, target, _ string) {
	m.reroutes.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("chain", chain), attribute.String("target", target)))
}

func (m *Metrics) NoHealthyTargets(chain string, _ int) {
	m.noHealthy.Add(context.Background(), 1, metric.WithAttributes(attribute.String("chain", chain)))
}

// UpstreamRequest counts the attempt per method and records its duration per
// target only. The histogram is 13 series (11 buckets, sum, count) per label
// set, so a method label there multiplied the whole stack's active series by
// the number of methods seen; per-method rates stay on the counter.
func (m *Metrics) UpstreamRequest(chain, target, method string, status int, duration time.Duration, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	chainAttr, targetAttr, outcomeAttr := attribute.String("chain", chain), attribute.String("target", target), attribute.String("outcome", outcome)
	ctx := context.Background()
	m.requests.Add(ctx, 1, metric.WithAttributes(chainAttr, targetAttr,
		attribute.String("method", NormalizeMethod(method, status)), outcomeAttr))
	m.duration.Record(ctx, duration.Seconds(), metric.WithAttributes(chainAttr, targetAttr, outcomeAttr))
}

// Placeholders NormalizeMethod substitutes for values a client controls.
const (
	methodUnknown     = "unknown"        // no JSON-RPC method and no path
	methodOther       = "other"          // a JSON-RPC method name that is not an identifier
	verbOther         = "OTHER"          // a non-standard HTTP verb
	pathClientError   = "{client_error}" // the target rejected the path (4xx other than 429)
	segmentIdentifier = "{id}"
)

// NormalizeMethod keeps the method attribute low-cardinality: every value it
// returns is either from a small fixed set or something a target accepted.
//
//   - JSON-RPC method names are used as-is when they look like identifiers
//     (letters, digits, underscore, at most 64 characters), "other" otherwise.
//   - HTTP paths (pass-through chains) are cut to "VERB /seg1/seg2"; any
//     id-looking segment (all digits, or a long token with digits: hashes,
//     addresses, ledger numbers) or segment that is not a plain word becomes
//     "{id}", so /accounts/GABC..., /ledgers/4501679 or /v2/accounts/<addr>
//     never create one series per account.
//   - A path the target answered with a client error (4xx other than 429) is
//     reported as "VERB {client_error}": such paths are whatever the client
//     typed (scanners probe /accounts/169.254.169.254, /etc/passwd...), and
//     each distinct one would otherwise stay an active series until restart.
func NormalizeMethod(method string, status int) string {
	// The proxy builds "VERB /path" for non-JSON-RPC requests; a JSON-RPC
	// method name with a space in it is still a method name.
	verb, path, isPath := strings.Cut(method, " ")
	if !isPath || !strings.HasPrefix(path, "/") {
		switch {
		case method == "":
			return methodUnknown
		case isIdentifier(method):
			return method
		default:
			return methodOther
		}
	}
	if !standardVerbs[verb] {
		verb = verbOther
	}
	if status >= 400 && status < 500 && status != 429 {
		return verb + " " + pathClientError
	}
	path, _, _ = strings.Cut(path, "?")
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) > 2 {
		segments = segments[:2]
	}
	if len(segments) == 1 && segments[0] == "" {
		return verb + " /"
	}
	for i, seg := range segments {
		if looksLikeID(seg) || !isWord(seg) {
			segments[i] = segmentIdentifier
		}
	}
	return fmt.Sprintf("%s /%s", verb, strings.Join(segments, "/"))
}

var standardVerbs = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true,
}

// isIdentifier: a JSON-RPC method name such as eth_getLogs or getAccountInfo.
func isIdentifier(s string) bool {
	if len(s) > 64 || !isLetter(rune(s[0])) {
		return false
	}
	for _, r := range s {
		if !isLetter(r) && !isDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

// isWord: a path segment such as fee_stats, strict-send, v2 or
// sui.rpc.v2.LedgerService. Anything with quotes, colons, spaces, $ or ` is
// not one, and neither is anything longer than 64 characters.
func isWord(s string) bool {
	if s == "" || len(s) > 64 || !isLetter(rune(s[0])) {
		return false
	}
	for _, r := range s {
		if !isLetter(r) && !isDigit(r) && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func isLetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }
func isDigit(r rune) bool  { return r >= '0' && r <= '9' }

// looksLikeID is deliberately simple: purely numeric segments, long segments
// that mix letters and digits (hex hashes, base58/base32 addresses, Stellar
// strkeys), and anything of 32 characters or more (an all-letter strkey such as
// GAAA...WHF). Short versioned segments such as "v2" stay as they are.
func looksLikeID(seg string) bool {
	if seg == "" {
		return false
	}
	if len(seg) >= 32 {
		return true
	}
	digits, letters := 0, 0
	for _, r := range seg {
		switch {
		case isDigit(r):
			digits++
		case isLetter(r):
			letters++
		}
	}
	if digits == len(seg) {
		return true
	}
	return len(seg) >= 16 && digits > 0 && letters > 0
}

func clampInt64(v uint64) int64 {
	if v > 1<<63-1 {
		return 1<<63 - 1
	}
	return int64(v)
}
