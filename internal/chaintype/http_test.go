package chaintype

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This file is an internal test (package chaintype) because it exercises the
// unexported helpers every chain type builds its Head on. Tests that need the
// fake node live in the external package chaintype_test instead.

func TestGetJSON(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		_, _ = w.Write([]byte(`{"round":7}`))
	}))
	defer srv.Close()

	body, err := getJSON(context.Background(), srv.Client(), srv.URL+"/v2/status?x=1", map[string]string{"X-Algo-API-Token": "tok"})
	if err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if string(body) != `{"round":7}` {
		t.Errorf("body = %s", body)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/v2/status" || got.URL.RawQuery != "x=1" {
		t.Errorf("unexpected request %s %s?%s", got.Method, got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("X-Algo-API-Token") != "tok" {
		t.Error("the target's headers must be sent")
	}
	if got.Header.Get("User-Agent") != healthCheckUserAgent {
		t.Errorf("user agent = %q", got.Header.Get("User-Agent"))
	}
}

func TestGetJSON_NonOKStatusIsAnErrorNamingTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
	}))
	defer srv.Close()

	_, err := getJSON(context.Background(), srv.Client(), srv.URL, nil)
	if err == nil {
		t.Fatal("a non-200 status must be an error")
	}
	if !strings.Contains(err.Error(), "http status 429") || !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("error must name the status and the body: %v", err)
	}
}

func TestPostJSON_SendsTheBodyAndHeaders(t *testing.T) {
	var gotBody, gotType, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody, gotType, gotKey = string(buf), r.Header.Get("Content-Type"), r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := postJSON(context.Background(), srv.Client(), srv.URL, map[string]string{"X-Api-Key": "k"}, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	if gotBody != `{"a":1}` || gotType != "application/json" || gotKey != "k" {
		t.Errorf("body=%q content-type=%q key=%q", gotBody, gotType, gotKey)
	}
}

// A provider that streams gigabytes must not exhaust the gateway's memory.
func TestResponseBodyIsSizeLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := strings.Repeat("x", 1<<16)
		for i := 0; i < 32; i++ { // 2 MiB, twice the limit
			_, _ = w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	body, err := getJSON(context.Background(), srv.Client(), srv.URL, nil)
	if err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if len(body) != maxResponseBytes {
		t.Errorf("body of %d bytes, want it cut at %d", len(body), maxResponseBytes)
	}
}

func TestJoinURLPath(t *testing.T) {
	tests := []struct{ base, sub, want string }{
		{"https://n.example", "/v2/status", "https://n.example/v2/status"},
		{"https://n.example/", "/v2/status", "https://n.example/v2/status"},
		{"https://n.example/base", "v2/status", "https://n.example/base/v2/status"},
		{"https://n.example/base/", "/v2/status", "https://n.example/base/v2/status"},
		{"https://n.example/base", "", "https://n.example/base"},
		{"https://n.example/base", "/", "https://n.example/base"},
	}
	for _, tt := range tests {
		if got := JoinURLPath(tt.base, tt.sub); got != tt.want {
			t.Errorf("JoinURLPath(%q, %q) = %q, want %q", tt.base, tt.sub, got, tt.want)
		}
	}
}

func TestParseHexUint64(t *testing.T) {
	ok := map[string]uint64{"0x0": 0, "0x1": 1, "0xaa36a7": 11155111, "0XFF": 255}
	for in, want := range ok {
		got, err := parseHexUint64(in)
		if err != nil || got != want {
			t.Errorf("parseHexUint64(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "12345", "0x", "0xzz", "0x10000000000000000"} {
		if _, err := parseHexUint64(in); err == nil {
			t.Errorf("parseHexUint64(%q) must fail", in)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc..." {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 3); got != "abc" {
		t.Errorf("truncate must leave short strings alone: %q", got)
	}
}
