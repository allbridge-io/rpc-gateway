//go:build testnet

// Package testnet runs the real gateway against public testnets.
//
//	make test-testnet
//
// Every chain from config.testnet.toml gets one extra, deliberately dead target
// injected. The tests then prove for each chain type that real calls succeed
// through the gateway and that the dead target never receives traffic.
//
// What "real calls succeed" means per chain type lives in checks_<type>_test.go,
// which registers a typeChecks in its init(); see checks_test.go.
package testnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/gateway"
)

const (
	deadTargetName = "DeadInjected"
	requestTimeout = 25 * time.Second
	wsTimeout      = 30 * time.Second
)

type env struct {
	cfg  *config.Config
	gw   *gateway.Gateway
	base string
	rec  *events.Recorder
	http *http.Client
}

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	path := os.Getenv("TESTNET_CONFIG_TOML_PATH")
	if path == "" {
		path = "config.testnet.toml"
	}
	cfg, err := config.Load(config.LoadOptions{
		ConfigPath:       path,
		SecretConfigPath: os.Getenv("SECRET_CONFIG_TOML_PATH"),
	})
	if err != nil {
		t.Fatalf("load testnet config: %v", err)
	}
	cfg.Server.Port = 0 // OS-assigned free port
	for key, chain := range cfg.Chains {
		chain.Targets = append(chain.Targets, config.Target{Name: deadTargetName, HTTPURL: "http://127.0.0.1:9"})
		cfg.Chains[key] = chain
	}
	if only := os.Getenv("TESTNET_CHAINS"); only != "" {
		keep := map[string]bool{}
		for _, k := range strings.Split(only, ",") {
			keep[strings.ToUpper(strings.TrimSpace(k))] = true
		}
		for key := range cfg.Chains {
			if !keep[strings.ToUpper(key)] {
				delete(cfg.Chains, key)
			}
		}
	}
	return cfg
}

// startGateway loads the testnet config, applies the optional mutators and
// runs the gateway on a free port until the test ends. Events go both to the
// log (visible with -v) and to a Recorder the assertions read.
func startGateway(t *testing.T, mutate ...func(*config.Config)) *env {
	t.Helper()
	cfg := loadConfig(t)
	for _, fn := range mutate {
		fn(cfg)
	}
	logger, _ := zap.NewDevelopment()
	rec := &events.Recorder{}
	gw, err := gateway.New(cfg, logger, events.Multi{rec, events.NewLogger(logger)})
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("gateway did not shut down in time")
		}
	})
	addrCtx, addrCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer addrCancel()
	addr, err := gw.Addr(addrCtx)
	if err != nil {
		t.Fatalf("gateway did not start (initial health round against testnets timed out?): %v", err)
	}
	t.Logf("gateway listening on %s with chains %v", addr, cfg.ChainKeys())
	return &env{cfg: cfg, gw: gw, base: "http://" + addr, rec: rec, http: &http.Client{Timeout: requestTimeout}}
}

// --- helpers ---

func (e *env) do(t *testing.T, method, path string, body []byte, headers ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpc performs a JSON-RPC call through the gateway and fails the test on any error.
func (e *env) rpc(t *testing.T, path, method string, params ...any) (json.RawMessage, *http.Response) {
	t.Helper()
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, data := e.do(t, http.MethodPost, path, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: HTTP %d %s", path, method, resp.StatusCode, truncate(data))
	}
	var parsed rpcResp
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("%s %s: not JSON-RPC: %s", path, method, truncate(data))
	}
	if parsed.Error != nil {
		t.Fatalf("%s %s: json-rpc error %d: %s", path, method, parsed.Error.Code, parsed.Error.Message)
	}
	if provider := resp.Header.Get("X-Rpc-Provider"); provider == "" || provider == deadTargetName {
		t.Fatalf("%s %s: served by %q", path, method, provider)
	}
	return parsed.Result, resp
}

func hexToUint(t *testing.T, raw json.RawMessage) uint64 {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("expected hex string, got %s", raw)
	}
	var v uint64
	if _, err := fmt.Sscanf(strings.TrimPrefix(s, "0x"), "%x", &v); err != nil {
		t.Fatalf("not hex: %s", s)
	}
	return v
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}

// --- the test ---

func TestTestnet(t *testing.T) {
	e := startGateway(t)

	t.Run("status", func(t *testing.T) {
		resp, data := e.do(t, http.MethodGet, "/status", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var st gateway.Status
		if err := json.Unmarshal(data, &st); err != nil {
			t.Fatalf("status JSON: %v", err)
		}
		for _, key := range e.cfg.ChainKeys() {
			c, ok := st.Chains[key]
			if !ok {
				t.Errorf("%s missing from status", key)
				continue
			}
			if c.RoutableTargets < 1 {
				t.Errorf("%s: no routable targets: %+v", key, c.Targets)
			}
			for _, target := range c.Targets {
				t.Logf("%s/%s routable=%v block=%d lag=%d err=%q", key, target.Name, target.Routable, target.BlockNumber, target.Lag, target.LastError)
				if target.Name == deadTargetName && (target.Routable || target.LastError == "") {
					t.Errorf("%s: injected dead target must be unhealthy after the initial round: %+v", key, target)
				}
				if target.Name != deadTargetName && target.Routable && target.BlockNumber == 0 {
					t.Errorf("%s/%s: routable but no block number", key, target.Name)
				}
			}
		}
	})

	for _, key := range e.cfg.ChainKeys() {
		key := key
		chain := e.cfg.Chains[key]
		t.Run(key, func(t *testing.T) {
			checksFor(t, chain.Type).Verify(t, e, key, chain)
		})
	}

	t.Run("dead target never used", func(t *testing.T) {
		if ev := e.rec.Of(events.KindRerouted, deadTargetName); len(ev) != 0 {
			t.Errorf("requests were routed to the dead target and had to be rerouted: %+v", ev)
		}
		if ev := e.rec.Of(events.KindNoHealthy, ""); len(ev) != 0 {
			t.Errorf("some chain had no healthy targets: %+v", ev)
		}
	})
}

func mustRaw(raw json.RawMessage, _ *http.Response) json.RawMessage { return raw }

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// TestTestnetReroute proves failover on the real network: the dead target is
// kept "healthy" by a huge failure threshold, so requests do land on it and
// must be rerouted to a live provider, after which the taint keeps it out.
func TestTestnetReroute(t *testing.T) {
	e := startGateway(t, func(cfg *config.Config) {
		cfg.HealthChecks.FailureThreshold = 1000 // never mark the dead target unhealthy by checks
		// Long enough that even ten slow testnet calls cannot outlive the taint.
		taint := 5 * time.Minute
		cfg.HealthChecks.TaintDuration = &taint
	})
	rec, gw, cfg := e.rec, e.gw, e.cfg

	for _, key := range cfg.ChainKeys() {
		key := key
		chain := cfg.Chains[key]
		t.Run(key, func(t *testing.T) {
			probe := checksFor(t, chain.Type).Probe
			call := func() *http.Response { return probe(t, e, key) }

			reroutesOfChain := func() []events.Event {
				var out []events.Event
				for _, ev := range rec.Of(events.KindRerouted, deadTargetName) {
					if ev.Chain == key {
						out = append(out, ev)
					}
				}
				return out
			}

			// With two targets picked at random, 30 tries are all but guaranteed to hit the dead one.
			var mine []events.Event
			for i := 0; i < 30 && len(mine) == 0; i++ {
				if provider := call().Header.Get("X-Rpc-Provider"); provider == deadTargetName {
					t.Fatalf("response claims to come from the dead target")
				}
				mine = reroutesOfChain()
			}
			if len(mine) == 0 {
				t.Fatal("the dead target was never picked; cannot observe the reroute")
			}
			t.Logf("%s: request hit %s and was rerouted: %s", key, deadTargetName, mine[0].Reason)
			if !strings.Contains(mine[0].Reason, "connection refused") {
				t.Errorf("unexpected reroute reason: %s", mine[0].Reason)
			}

			// The failed attempt tainted the dead target: the next requests must skip it.
			var tainted bool
			for _, s := range gw.Chain(key).Manager.Status() {
				if s.Name == deadTargetName {
					tainted = s.Tainted
				}
			}
			if !tainted {
				t.Fatalf("dead target must be tainted after the failed request")
			}
			before := len(reroutesOfChain())
			for i := 0; i < 10; i++ {
				call()
			}
			if after := len(reroutesOfChain()); after != before {
				t.Errorf("tainted target still received requests: %d new reroutes", after-before)
			}
		})
	}
}
