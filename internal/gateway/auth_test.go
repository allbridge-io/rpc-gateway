package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	testKey    = "0123456789abcdef0123456789abcdef"
	testKeyTwo = "second-key-fedcba9876543210"
)

func newAuthFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWith(t, "", "[server]\napi_keys = [\""+testKey+"\", \""+testKeyTwo+"\"]\n")
}

func TestAuth_KeyInPathUnlocksEveryRoute(t *testing.T) {
	f := newAuthFixture(t)

	for _, key := range []string{testKey, testKeyTwo} {
		resp, body := f.do(t, http.MethodPost, "/"+key+"/SPL", rpcBody)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(nodeOf(t, body), "Spl") {
			t.Errorf("POST /%s/SPL: %d %s", key, resp.StatusCode, body)
		}
		resp, body = f.do(t, http.MethodPost, "/"+key+"/sol/", rpcBody)
		if resp.StatusCode != http.StatusOK || nodeOf(t, body) != "Sol" {
			t.Errorf("POST /%s/sol/: %d %s", key, resp.StatusCode, body)
		}
	}
	// Pass-through chains keep their sub-path and query behind the key.
	resp, body := f.do(t, http.MethodGet, "/"+testKey+"/TRX/v1/accounts/TAbc/transactions?limit=2", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"path":"/v1/accounts/TAbc/transactions"`) || !strings.Contains(body, `"query":"limit=2"`) {
		t.Errorf("pass-through behind the key: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(t, http.MethodPost, "/"+testKey+"/TRX", "{}")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"path":"/"`) {
		t.Errorf("bare /{key}/TRX must reach the target root: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.do(t, http.MethodOptions, "/"+testKey+"/SPL", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS behind the key: %d", resp.StatusCode)
	}
	resp, body = f.do(t, http.MethodGet, "/"+testKey+"/status", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"SPL"`) {
		t.Errorf("GET /{key}/status: %d %s", resp.StatusCode, body)
	}
	// The key never reaches the upstream.
	for _, c := range append(f.spl1.Calls(), f.trx.Calls()...) {
		if strings.Contains(c.Path, testKey) {
			t.Errorf("api key forwarded to the target: %s", c.Path)
		}
	}
}

func TestAuth_MissingOrWrongKeyIs401(t *testing.T) {
	f := newAuthFixture(t)

	wrong := strings.ToUpper(testKey) // same length, different bytes
	for _, path := range []string{
		"/SPL", "/sol", "/status", "/TRX/wallet/getnowblock", "/",
		"/" + wrong + "/SPL", "/" + testKey[:len(testKey)-1] + "/SPL", "/" + testKey + "x/SPL",
		"/nokey/status", "/" + testKey + "x",
	} {
		resp, body := f.do(t, http.MethodPost, path, rpcBody)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", path, resp.StatusCode)
		}
		if !strings.Contains(body, `"error"`) || !strings.Contains(body, "API key") {
			t.Errorf("%s: expected a JSON-RPC style error about the key, got %s", path, body)
		}
		if strings.Contains(body, "SPL") || strings.Contains(body, testKey) {
			t.Errorf("%s: a rejected request must not learn the chains or the key: %s", path, body)
		}
	}
	if f.spl1.CallCount("eth_call")+f.spl2.CallCount("eth_call")+f.sol.CallCount("eth_call") != 0 {
		t.Error("rejected requests must never reach a target")
	}
}

func TestAuth_HealthzStaysPublic(t *testing.T) {
	f := newAuthFixture(t)
	resp, body := f.do(t, http.MethodGet, "/healthz", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"healthy":true`) {
		t.Errorf("/healthz without a key (Render health check): %d %s", resp.StatusCode, body)
	}
	// ... but it is not served behind the key: nothing there is a chain.
	resp, _ = f.do(t, http.MethodGet, "/"+testKey+"/healthz", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/{key}/healthz: %d", resp.StatusCode)
	}
}

func TestAuth_DisabledWithoutKeys(t *testing.T) {
	f := newFixture(t, "")
	resp, _ := f.do(t, http.MethodPost, "/SPL", rpcBody)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("no api_keys: /SPL must stay open, got %d", resp.StatusCode)
	}
	resp, _ = f.do(t, http.MethodPost, "/"+testKey+"/SPL", rpcBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("no api_keys: a key prefix is just an unknown chain, got %d", resp.StatusCode)
	}
}

// The key must not appear in the request log, neither on accepted requests
// (the path is logged after the prefix is stripped) nor on rejected ones.
func TestAuth_KeyNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&buf), zapcore.DebugLevel)
	f := newAuthFixture(t)
	gw, err := New(f.gw.cfg, zap.New(core), f.rec)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Close()
	f.srv = httptest.NewServer(gw.Handler())
	t.Cleanup(f.srv.Close)

	f.do(t, http.MethodPost, "/"+testKey+"/SPL", rpcBody)
	f.do(t, http.MethodGet, "/"+testKey+"/TRX/wallet/getnowblock", "")
	f.do(t, http.MethodPost, "/"+testKey+"x/SPL", rpcBody)
	f.do(t, http.MethodPost, "/"+testKey[:20]+"/SPL", rpcBody)

	logs := buf.String()
	if !strings.Contains(logs, `"path":"/SPL"`) || !strings.Contains(logs, `"path":"/wallet/getnowblock"`) {
		t.Errorf("accepted requests should be logged with the stripped path:\n%s", logs)
	}
	if !strings.Contains(logs, `"path":"/<invalid-key>/SPL"`) {
		t.Errorf("rejected requests should be logged with a placeholder:\n%s", logs)
	}
	for _, secret := range []string{testKey, testKey[:20]} {
		if strings.Contains(logs, secret) {
			t.Errorf("log leaks the key %q:\n%s", secret, logs)
		}
	}
}
