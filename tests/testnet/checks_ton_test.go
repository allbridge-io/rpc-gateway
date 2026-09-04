//go:build testnet

package testnet

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/testutil/fakenode"
)

func init() {
	registerChecks(fakenode.TypeTON, typeChecks{
		Verify: testTON,
		Probe: func(t *testing.T, e *env, key string) *http.Response {
			tonPace(e, key)
			resp, data := e.do(t, http.MethodGet, "/"+key+"/api/v3/masterchainInfo", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
			}
			return resp
		},
	})
}

// toncenter allows about one request per second without an API key and answers
// 429 above it. A 429 is a node-side failure for the gateway: it taints the
// only real target of the chain, so the next call gets a 503 and the run fails
// on a rate limit instead of on a real defect. Everything this file sends is
// therefore paced.
//
// Our own calls are not the whole load: the gateway health-checks the same
// target every healthchecks.interval, and a call landing within a second of a
// check is what actually triggered the 429s. tonPace keeps a minimum gap
// between our calls *and* stays clear of the check schedule, which it reads
// from the manager's LastCheck.
const (
	tonMinGap      = 1200 * time.Millisecond
	tonHealthGuard = 1300 * time.Millisecond
)

var tonPacer struct {
	mu   sync.Mutex
	last time.Time
}

func tonPace(e *env, key string) {
	tonPacer.mu.Lock()
	defer tonPacer.mu.Unlock()

	if wait := time.Until(tonPacer.last.Add(tonMinGap)); wait > 0 {
		time.Sleep(wait)
	}
	// Health checks run on a ticker, so the last one dates the whole series.
	// Sleep out of the danger zone around the check that is due next.
	if lastCheck := tonLastCheck(e, key); !lastCheck.IsZero() {
		if interval := e.cfg.HealthChecks.Interval; interval > 3*tonHealthGuard {
			switch phase := time.Since(lastCheck) % interval; {
			case phase < tonHealthGuard:
				time.Sleep(tonHealthGuard - phase)
			case interval-phase < tonHealthGuard:
				time.Sleep(interval - phase + tonHealthGuard)
			}
		}
	}
	tonPacer.last = time.Now()
}

// tonLastCheck is when the chain was last health-checked, zero if unknown.
func tonLastCheck(e *env, key string) time.Time {
	chain := e.gw.Chain(key)
	if chain == nil {
		return time.Time{}
	}
	var last time.Time
	for _, s := range chain.Manager.Status() {
		if s.LastCheck.After(last) {
			last = s.LastCheck
		}
	}
	return last
}

// tonGet performs one paced GET through the gateway.
func tonGet(t *testing.T, e *env, key, path string) (*http.Response, []byte) {
	t.Helper()
	tonPace(e, key)
	return e.do(t, http.MethodGet, path, nil)
}

// tonReroutes returns the reroutes recorded for one chain.
func tonReroutes(e *env, key string) []events.Event {
	var out []events.Event
	for _, ev := range e.rec.Of(events.KindRerouted, "") {
		if ev.Chain == key {
			out = append(out, ev)
		}
	}
	return out
}

func testTON(t *testing.T, e *env, key string, _ config.Chain) {
	path := "/" + key
	var v3Seqno uint64

	t.Run("v3 masterchainInfo", func(t *testing.T) {
		resp, data := tonGet(t, e, key, path+"/api/v3/masterchainInfo")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		var info struct {
			Last struct {
				Workchain int    `json:"workchain"`
				Seqno     uint64 `json:"seqno"`
			} `json:"last"`
		}
		if err := json.Unmarshal(data, &info); err != nil {
			t.Fatalf("not JSON: %s", truncate(data))
		}
		if info.Last.Seqno == 0 {
			t.Fatalf("no last.seqno in %s", truncate(data))
		}
		if info.Last.Workchain != -1 {
			t.Errorf("last block must be a masterchain block, got workchain %d", info.Last.Workchain)
		}
		v3Seqno = info.Last.Seqno
		t.Logf("%s masterchain seqno=%d provider=%s", key, v3Seqno, resp.Header.Get("X-Rpc-Provider"))
	})

	t.Run("v2 jsonRPC getMasterchainInfo", func(t *testing.T) {
		tonPace(e, key)
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"getMasterchainInfo","params":{}}`)
		resp, data := e.do(t, http.MethodPost, path+"/api/v2/jsonRPC", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		var parsed struct {
			OK     bool `json:"ok"`
			Result struct {
				Last struct {
					Seqno uint64 `json:"seqno"`
				} `json:"last"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("not JSON: %s", truncate(data))
		}
		if !parsed.OK || parsed.Result.Last.Seqno == 0 {
			t.Fatalf("no result.last.seqno in %s", truncate(data))
		}
		// The same node answered both APIs moments apart: the two heads must
		// agree up to the blocks produced in between (~5s per masterchain block).
		if v3Seqno != 0 && absDiff(v3Seqno, parsed.Result.Last.Seqno) > 10 {
			t.Errorf("jsonRPC seqno %d is far from the v3 seqno %d", parsed.Result.Last.Seqno, v3Seqno)
		}
		if p := resp.Header.Get("X-Rpc-Provider"); p == "" || p == deadTargetName {
			t.Errorf("served by %q", p)
		}
	})

	t.Run("v3 blocks", func(t *testing.T) {
		resp, data := tonGet(t, e, key, path+"/api/v3/blocks?limit=1")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d %s", resp.StatusCode, truncate(data))
		}
		var parsed struct {
			Blocks []struct {
				Seqno uint64 `json:"seqno"`
			} `json:"blocks"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil || len(parsed.Blocks) == 0 {
			t.Fatalf("no blocks in %s", truncate(data))
		}
		if parsed.Blocks[0].Seqno == 0 {
			t.Errorf("block without a seqno: %s", truncate(data))
		}
	})

	t.Run("client error passed through", func(t *testing.T) {
		// Count only this chain's reroutes, and only the ones this very call
		// could have caused: the recorder is shared by every chain of the run.
		before := len(tonReroutes(e, key))
		resp, data := tonGet(t, e, key, path+"/api/v3/addressInformation?address=bad")
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("expected a 4xx from toncenter, got HTTP %d %s", resp.StatusCode, truncate(data))
		}
		if !strings.Contains(string(data), `"error"`) {
			t.Errorf("toncenter's error body must be passed through: %s", truncate(data))
		}
		if after := tonReroutes(e, key); len(after) != before {
			t.Errorf("client error must not cause a reroute: %+v", after[before:])
		}
	})
}
