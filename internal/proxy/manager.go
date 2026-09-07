package proxy

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/0xProject/rpc-gateway/internal/chaintype"
	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
)

// HealthOptions configures a Manager.
type HealthOptions struct {
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold uint
	SuccessThreshold uint
	// MaxBlockLag marks a target unhealthy when it is more than this many blocks
	// behind the best target of the chain. 0 disables the check.
	MaxBlockLag uint64
	// TaintDuration is how long Taint excludes a target. 0 disables tainting.
	TaintDuration time.Duration
	// Client performs health check calls. Optional.
	Client *http.Client
	// Now returns the current time. Optional, for tests.
	Now func() time.Time
}

// TargetStatus is a snapshot of one target, used by the /status endpoint and tests.
type TargetStatus struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
	// Routable is the final verdict: requests are sent to the target.
	Routable bool `json:"routable"`
	// CheckHealthy reflects consecutive check results only.
	CheckHealthy        bool      `json:"checkHealthy"`
	Lagging             bool      `json:"lagging"`
	Tainted             bool      `json:"tainted"`
	TaintReason         string    `json:"taintReason,omitempty"`
	BlockNumber         uint64    `json:"blockNumber"`
	Lag                 uint64    `json:"lag"`
	LastError           string    `json:"lastError,omitempty"`
	LastCheck           time.Time `json:"lastCheck"`
	ConsecutiveFailures uint      `json:"consecutiveFailures"`
}

type targetState struct {
	cfg config.Target

	mu           sync.Mutex
	healthy      bool // by consecutive check results
	lagging      bool
	taintedUntil time.Time
	taintReason  string
	block        uint64
	lag          uint64
	lastErr      string
	lastCheck    time.Time
	failures     uint
	successes    uint
}

type checkResult struct {
	block uint64
	err   error
}

// Manager runs health checks for the targets of one chain and answers the
// question "which target may receive the next request?".
//
// A target is routable when all of these hold:
//   - it is not disabled in the config
//   - its last FailureThreshold checks were not all failures (or it recovered
//     with SuccessThreshold successes)
//   - it is not lagging behind the best target by more than MaxBlockLag
//   - it is not tainted by a recent failed request
type Manager struct {
	chain string
	typ   config.ChainType
	// spec is the chain type resolved once at construction; specOK is false for
	// a type no build of the gateway knows, which config validation rejects.
	spec     chaintype.Spec
	specOK   bool
	opts     HealthOptions
	targets  []*targetState
	client   *http.Client
	observer events.Observer
	now      func() time.Time
	redact   *redactor
}

// NewManager creates a Manager. Targets start optimistic (healthy) until the
// first check round says otherwise; call RunOnce before serving traffic.
func NewManager(chain string, typ config.ChainType, targets []config.Target, opts HealthOptions, observer events.Observer) *Manager {
	if observer == nil {
		observer = events.Nop{}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: opts.Timeout}
	}
	if opts.FailureThreshold == 0 {
		opts.FailureThreshold = 1
	}
	if opts.SuccessThreshold == 0 {
		opts.SuccessThreshold = 1
	}
	spec, specOK := chaintype.Lookup(string(typ))
	m := &Manager{
		chain:    chain,
		typ:      typ,
		spec:     spec,
		specOK:   specOK,
		opts:     opts,
		client:   opts.Client,
		observer: observer,
		now:      opts.Now,
		redact:   newRedactor(targets),
	}
	for _, t := range targets {
		m.targets = append(m.targets, &targetState{cfg: t, healthy: true})
	}
	return m
}

// Len returns the number of targets, disabled ones included.
func (m *Manager) Len() int { return len(m.targets) }

// Name returns the name of target i.
func (m *Manager) Name(i int) string { return m.targets[i].cfg.Name }

// Start runs a check round immediately and then every Interval until ctx ends.
// Callers that already ran an initial round must use StartTicker instead, so
// targets are not checked twice within milliseconds at startup.
func (m *Manager) Start(ctx context.Context) {
	m.RunOnce(ctx)
	m.StartTicker(ctx)
}

// StartTicker runs a check round every Interval until ctx ends, without an
// immediate one.
func (m *Manager) StartTicker(ctx context.Context) {
	ticker := time.NewTicker(m.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.RunOnce(ctx)
		}
	}
}

// RunOnce checks every enabled target concurrently, then updates health and
// lag states and emits events for every transition. It is synchronous so tests
// can drive it deterministically.
func (m *Manager) RunOnce(ctx context.Context) {
	results := make([]checkResult, len(m.targets))
	var wg sync.WaitGroup
	for i, t := range m.targets {
		if t.cfg.Disabled {
			continue
		}
		wg.Add(1)
		go func(i int, t *targetState) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
			defer cancel()
			block, err := m.head(cctx, t.cfg)
			results[i] = checkResult{block: block, err: err}
		}(i, t)
	}
	wg.Wait()

	now := m.now()
	var best uint64
	for i, t := range m.targets {
		if t.cfg.Disabled {
			continue
		}
		m.recordCheck(t, results[i], now)
		if results[i].err == nil && results[i].block > best {
			best = results[i].block
		}
	}
	for i, t := range m.targets {
		if t.cfg.Disabled {
			continue
		}
		m.updateLag(t, results[i], best)
	}
}

// head performs one health check call for a target through the chain type's
// registered check. An unknown type fails the check instead of panicking: the
// gateway keeps serving its other chains and /status shows why.
func (m *Manager) head(ctx context.Context, t config.Target) (uint64, error) {
	if !m.specOK {
		return 0, fmt.Errorf("unknown chain type %q; known types: %s", m.typ, strings.Join(chaintype.Names(), ", "))
	}
	return m.spec.Head(ctx, m.client, t.HTTPURL, t.Headers)
}

func (m *Manager) recordCheck(t *targetState, r checkResult, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastCheck = now
	if r.err != nil {
		t.failures++
		t.successes = 0
		t.lastErr = m.redact.String(r.err.Error())
		if t.healthy && t.failures >= m.opts.FailureThreshold {
			t.healthy = false
			reason := fmt.Sprintf("%d consecutive failed checks, last: %s", t.failures, t.lastErr)
			m.emitHealth(t, reason)
		}
		return
	}
	t.successes++
	t.failures = 0
	t.lastErr = ""
	t.block = r.block
	if !t.healthy && t.successes >= m.opts.SuccessThreshold {
		t.healthy = true
		m.emitHealth(t, fmt.Sprintf("recovered after %d consecutive successful checks", t.successes))
	}
}

func (m *Manager) updateLag(t *targetState, r checkResult, best uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.err != nil {
		// Unknown block this round: lag is not judged, thresholds handle the failure.
		t.lag = 0
		if t.lagging {
			t.lagging = false
		}
		return
	}
	t.lag = best - r.block
	lagging := m.opts.MaxBlockLag > 0 && t.lag > m.opts.MaxBlockLag
	if lagging == t.lagging {
		return
	}
	t.lagging = lagging
	if !t.healthy {
		return // already reported as unhealthy by checks; no extra noise
	}
	if lagging {
		m.emitHealth(t, fmt.Sprintf("lagging %d blocks behind the best target (limit %d)", t.lag, m.opts.MaxBlockLag))
	} else {
		m.emitHealth(t, "caught up with the best target")
	}
}

// emitHealth reports the routable-by-checks state (healthy && !lagging). Caller holds t.mu.
func (m *Manager) emitHealth(t *targetState, reason string) {
	m.observer.TargetHealthChanged(m.chain, t.cfg.Name, t.healthy && !t.lagging, reason)
}

// IsRoutable tells whether target i may receive a request right now.
func (m *Manager) IsRoutable(i int) bool {
	t := m.targets[i]
	if t.cfg.Disabled {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.healthy && !t.lagging && !m.now().Before(t.taintedUntil)
}

// NextRoutable picks a random routable target whose index is not in excluded.
// Returns -1 when there is none.
func (m *Manager) NextRoutable(excluded []int) int {
	n := len(m.targets)
	if n == 0 {
		return -1
	}
	start := rand.IntN(n)
	for delta := 0; delta < n; delta++ {
		i := (start + delta) % n
		if contains(excluded, i) {
			continue
		}
		if m.IsRoutable(i) {
			return i
		}
	}
	return -1
}

// Taint excludes target i from routing for TaintDuration. No-op when disabled.
func (m *Manager) Taint(i int, reason string) {
	if m.opts.TaintDuration <= 0 {
		return
	}
	reason = m.redact.String(reason)
	t := m.targets[i]
	t.mu.Lock()
	until := m.now().Add(m.opts.TaintDuration)
	alreadyTainted := m.now().Before(t.taintedUntil)
	t.taintedUntil = until
	t.taintReason = reason
	t.mu.Unlock()
	if !alreadyTainted {
		m.observer.TargetTainted(m.chain, t.cfg.Name, reason, m.opts.TaintDuration)
	}
}

// Status returns a snapshot of every target.
func (m *Manager) Status() []TargetStatus {
	now := m.now()
	out := make([]TargetStatus, 0, len(m.targets))
	for i, t := range m.targets {
		t.mu.Lock()
		tainted := now.Before(t.taintedUntil)
		s := TargetStatus{
			Name:                t.cfg.Name,
			Disabled:            t.cfg.Disabled,
			CheckHealthy:        t.healthy,
			Lagging:             t.lagging,
			Tainted:             tainted,
			BlockNumber:         t.block,
			Lag:                 t.lag,
			LastError:           t.lastErr,
			LastCheck:           t.lastCheck,
			ConsecutiveFailures: t.failures,
		}
		if tainted {
			s.TaintReason = t.taintReason
		}
		t.mu.Unlock()
		s.Routable = m.IsRoutable(i)
		out = append(out, s)
	}
	return out
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
