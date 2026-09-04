//go:build testnet

// Package testnet runs the real gateway against public testnets.
//
//	make test-testnet
//
// Every chain from config.testnet.toml gets one extra, deliberately dead target
// injected. The tests then prove for each chain type that real calls succeed
// through the gateway and that the dead target never receives traffic.
package testnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
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

func startGateway(t *testing.T) *env {
	t.Helper()
	cfg := loadConfig(t)
	logger, _ := zap.NewDevelopment()
	rec := &events.Recorder{}
	gw, err := gateway.New(cfg, logger, multiObserver{rec, events.NewLogger(logger)})
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

// multiObserver fans events out to a recorder (for assertions) and the log.
type multiObserver []events.Observer

func (m multiObserver) TargetHealthChanged(c, t string, h bool, r string) {
	for _, o := range m {
		o.TargetHealthChanged(c, t, h, r)
	}
}
func (m multiObserver) TargetTainted(c, t, r string, d time.Duration) {
	for _, o := range m {
		o.TargetTainted(c, t, r, d)
	}
}
func (m multiObserver) RequestRerouted(c, t, r string) {
	for _, o := range m {
		o.RequestRerouted(c, t, r)
	}
}
func (m multiObserver) NoHealthyTargets(c string, n int) {
	for _, o := range m {
		o.NoHealthyTargets(c, n)
	}
}
func (m multiObserver) UpstreamRequest(c, t, meth string, s int, d time.Duration, err error) {
	for _, o := range m {
		o.UpstreamRequest(c, t, meth, s, d, err)
	}
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
			switch chain.Type {
			case config.ChainTypeEVM:
				testEVM(t, e, key, chain)
			case config.ChainTypeSolana:
				testSolana(t, e, key)
			case config.ChainTypeTron:
				testTron(t, e, key, chain)
			default:
				t.Fatalf("no testnet checks for chain type %q", chain.Type)
			}
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

func testEVM(t *testing.T, e *env, key string, chain config.Chain) {
	path := "/" + key

	t.Run("eth_chainId", func(t *testing.T) {
		raw, resp := e.rpc(t, path, "eth_chainId")
		var id string
		_ = json.Unmarshal(raw, &id)
		t.Logf("%s chainId=%s provider=%s", key, id, resp.Header.Get("X-Rpc-Provider"))
		if chain.ChainID != "" && !strings.EqualFold(id, chain.ChainID) {
			t.Errorf("chain id %s, config expects %s: wrong network behind %s", id, chain.ChainID, key)
		}
	})
	t.Run("eth_blockNumber", func(t *testing.T) {
		block := hexToUint(t, mustRaw(e.rpc(t, path, "eth_blockNumber")))
		if block == 0 {
			t.Fatal("block number is zero")
		}
		st := e.gw.Chain(key).Manager.Status()
		var checked uint64
		for _, s := range st {
			if s.BlockNumber > checked {
				checked = s.BlockNumber
			}
		}
		if diff := absDiff(block, checked); diff > 200 {
			t.Errorf("block via proxy %d differs from health-check block %d by %d", block, checked, diff)
		}
	})
	t.Run("eth_getBalance", func(t *testing.T) {
		raw, _ := e.rpc(t, path, "eth_getBalance", "0x0000000000000000000000000000000000000000", "latest")
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatalf("expected hex string, got %s", raw)
		}
		if _, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16); !ok || !strings.HasPrefix(s, "0x") {
			t.Fatalf("balance is not a 0x hex quantity: %s", s)
		}
	})
	t.Run("eth_call revert is passed to the client", func(t *testing.T) {
		// Calling a non-contract address returns "0x"; a JSON-RPC error from a
		// real revert would also be passed through, not turned into a 503.
		raw, _ := e.rpc(t, path, "eth_call", map[string]string{"to": "0x0000000000000000000000000000000000000001", "data": "0x00"}, "latest")
		if !strings.HasPrefix(string(raw), `"0x`) {
			t.Errorf("unexpected eth_call result %s", raw)
		}
	})
	t.Run("repeated requests stay on healthy targets", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			e.rpc(t, path, "eth_blockNumber")
		}
	})
}

func testSolana(t *testing.T, e *env, key string) {
	path := "/" + key

	t.Run("getHealth", func(t *testing.T) {
		raw, resp := e.rpc(t, path, "getHealth")
		t.Logf("%s getHealth=%s provider=%s", key, raw, resp.Header.Get("X-Rpc-Provider"))
		if string(raw) != `"ok"` {
			t.Errorf("getHealth = %s", raw)
		}
	})
	t.Run("getSlot", func(t *testing.T) {
		var slot uint64
		if err := json.Unmarshal(mustRaw(e.rpc(t, path, "getSlot")), &slot); err != nil || slot == 0 {
			t.Fatalf("getSlot: %v", err)
		}
	})
	t.Run("getLatestBlockhash", func(t *testing.T) {
		raw, _ := e.rpc(t, path, "getLatestBlockhash", map[string]string{"commitment": "finalized"})
		if !strings.Contains(string(raw), `"blockhash"`) {
			t.Errorf("no blockhash in %s", raw)
		}
	})
	t.Run("websocket slotSubscribe", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), wsTimeout)
		defer cancel()
		wsURL := strings.Replace(e.base, "http://", "ws://", 1) + path
		c, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Fatalf("ws dial %s: %v", wsURL, err)
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":1,"method":"slotSubscribe"}`)); err != nil {
			t.Fatalf("ws write: %v", err)
		}
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatalf("ws read (no slotNotification within %v): %v", wsTimeout, err)
			}
			if strings.Contains(string(data), `"slotNotification"`) {
				t.Logf("received %s", truncate(data))
				return
			}
			if strings.Contains(string(data), `"error"`) {
				t.Fatalf("subscription error: %s", data)
			}
		}
	})
}

func testTron(t *testing.T, e *env, key string, chain config.Chain) {
	path := "/" + key

	t.Run("wallet getnowblock", func(t *testing.T) {
		resp, data := e.do(t, http.MethodPost, path+"/wallet/getnowblock", []byte("{}"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		var b struct {
			BlockHeader struct {
				RawData struct {
					Number uint64 `json:"number"`
				} `json:"raw_data"`
			} `json:"block_header"`
		}
		if err := json.Unmarshal(data, &b); err != nil || b.BlockHeader.RawData.Number == 0 {
			t.Fatalf("no block number in %s", truncate(data))
		}
		t.Logf("%s block=%d provider=%s", key, b.BlockHeader.RawData.Number, resp.Header.Get("X-Rpc-Provider"))
	})
	t.Run("wallet getaccount via GET with query", func(t *testing.T) {
		resp, data := e.do(t, http.MethodGet, path+"/wallet/getaccount?address=TJRabPrwbZy45sbavfcjinPJC18kjpRTv8&visible=true", nil)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"address"`) {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
	})
	t.Run("client error passed through", func(t *testing.T) {
		resp, data := e.do(t, http.MethodPost, path+"/wallet/getaccount", []byte(`{"address":"not-an-address","visible":true}`))
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"Error"`) {
			t.Fatalf("expected Tron's 200 + Error body, got HTTP %d %s", resp.StatusCode, truncate(data))
		}
		if ev := e.rec.Of(events.KindRerouted, ""); len(ev) != 0 {
			t.Errorf("client error must not cause a reroute: %+v", ev)
		}
	})
	t.Run("jsonrpc eth_chainId", func(t *testing.T) {
		raw, _ := e.rpc(t, path+"/jsonrpc", "eth_chainId")
		var id string
		_ = json.Unmarshal(raw, &id)
		if chain.ChainID != "" && !strings.EqualFold(id, chain.ChainID) {
			t.Errorf("chain id %s, config expects %s", id, chain.ChainID)
		}
	})
	t.Run("v1 events (TronGrid only)", func(t *testing.T) {
		resp, data := e.do(t, http.MethodGet, path+"/v1/blocks/latest/events?limit=1", nil)
		if resp.StatusCode == http.StatusNotFound {
			t.Skip("target is not TronGrid; /v1 API not available")
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"data"`) {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
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
	cfg := loadConfig(t)
	cfg.HealthChecks.FailureThreshold = 1000 // never mark the dead target unhealthy by checks
	taint := 30 * time.Second
	cfg.HealthChecks.TaintDuration = &taint

	logger, _ := zap.NewDevelopment()
	rec := &events.Recorder{}
	gw, err := gateway.New(cfg, logger, multiObserver{rec, events.NewLogger(logger)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.ListenAndServe(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	addrCtx, addrCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer addrCancel()
	addr, err := gw.Addr(addrCtx)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{cfg: cfg, gw: gw, base: "http://" + addr, rec: rec, http: &http.Client{Timeout: requestTimeout}}

	for _, key := range cfg.ChainKeys() {
		key := key
		chain := cfg.Chains[key]
		t.Run(key, func(t *testing.T) {
			path := "/" + key
			call := func() *http.Response {
				switch chain.Type {
				case config.ChainTypeTron:
					resp, data := e.do(t, http.MethodPost, path+"/wallet/getnowblock", []byte("{}"))
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
					}
					return resp
				case config.ChainTypeSolana:
					_, resp := e.rpc(t, path, "getSlot")
					return resp
				default:
					_, resp := e.rpc(t, path, "eth_blockNumber")
					return resp
				}
			}

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
