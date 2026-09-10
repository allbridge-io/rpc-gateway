package chaintype

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestDescribeBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "an nginx error page is reduced to its title",
			body: "<html>\r\n<head><title>503 Service Temporarily Unavailable</title></head>\r\n<body>\r\n<center><h1>503 Service Temporarily Unavailable</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n",
			want: "html page: 503 Service Temporarily Unavailable",
		},
		{
			name: "a heading that repeats the title is not repeated",
			body: "<html><head><title>Attention Required! | Cloudflare</title></head><body><h1><span>Attention Required!</span></h1></body></html>",
			want: "html page: Attention Required! | Cloudflare",
		},
		{
			name: "a heading that adds to the title is appended",
			body: "<html><head><title>Attention Required! | Cloudflare</title></head><body><h1><span>Error 1015</span> You are being rate limited</h1></body></html>",
			want: "html page: Attention Required! | Cloudflare — Error 1015 You are being rate limited",
		},
		{
			name: "a page without a title falls back to its heading",
			body: "<html><body><h2 class=\"x\">Bad Gateway</h2><p>the upstream said no</p></body></html>",
			want: "html page: Bad Gateway",
		},
		{
			name: "a page with neither falls back to its visible text",
			body: "<html><head><style>h1{}</style></head><body><script>var a=1;</script><p>Service   down</p><p>try later</p></body></html>",
			want: "html page: Service down try later",
		},
		{
			name: "html without a title says only that it is html",
			body: "<!DOCTYPE html><html><body>nope</body></html>",
			want: "html page: nope",
		},
		{
			name: "html with no text at all says only that it is html",
			body: "<html><body></body></html>",
			want: "html page",
		},
		{
			name: "a multi-line json error collapses to one line",
			body: "{\n  \"error\": \"rate limit\n exceeded\"\n}",
			want: "{ \"error\": \"rate limit exceeded\" }",
		},
		{
			name: "a long body is truncated",
			body: strings.Repeat("a", 300),
			want: strings.Repeat("a", 200) + "...",
		},
		{
			name: "a blank body is named as such",
			body: "  \n",
			want: "empty body",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := describeBody([]byte(tt.body)); got != tt.want {
				t.Errorf("describeBody = %q, want %q", got, tt.want)
			}
		})
	}
}

// An upstream behind a proxy answers with an HTML page; the error that reaches
// /status and the logs must carry one line, not the whole page.
func TestGetJSON_HTMLErrorPageIsCompacted(t *testing.T) {
	const page = "<html>\r\n<head><title>503 Service Temporarily Unavailable</title></head>\r\n<body>\r\n<center><h1>503 Service Temporarily Unavailable</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	_, err := getJSON(context.Background(), srv.Client(), srv.URL, nil)
	if err == nil {
		t.Fatal("a non-200 status must be an error")
	}
	if !strings.Contains(err.Error(), "http status 503: html page: 503 Service Temporarily Unavailable") {
		t.Errorf("error must name the status and the page title: %v", err)
	}
	if strings.Contains(err.Error(), "<html") {
		t.Errorf("the html itself must not reach the error: %v", err)
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

// A target URL often carries an API key; the *url.Error net/http returns on a
// failed request must not repeat it, while the cause stays inspectable.
func TestDoStripsURLSecretsFromClientErrors(t *testing.T) {
	const secret = "SECRET-KEY-789"
	client := &http.Client{Timeout: time.Second}
	_, err := getJSON(context.Background(), client, "http://127.0.0.1:9/v2/"+secret+"?api-key="+secret, nil)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error repeats the key: %v", err)
	}
	if !strings.Contains(err.Error(), "http://127.0.0.1:9") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("host and cause should survive: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	_, err = getJSON(ctx, client, "http://127.0.0.1:9/v2/"+secret, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wrapping must keep errors.Is working, got %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("timeout error repeats the key: %v", err)
	}
}

// params: nil leaves the field out (Soroban RPC rejects "params": []),
// an explicit empty slice sends it (EVM/Solana/Sui keep their old wire format).
func TestCallJSONRPCParamsEncoding(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok"}`))
	}))
	defer srv.Close()
	client := &http.Client{Timeout: time.Second}
	if _, err := callJSONRPC(context.Background(), client, srv.URL, nil, "m", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := callJSONRPC(context.Background(), client, srv.URL, nil, "m", []any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := callJSONRPC(context.Background(), client, srv.URL, nil, "m", []any{"0x1", true}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"jsonrpc":"2.0","id":1,"method":"m"}`,
		`{"jsonrpc":"2.0","id":1,"method":"m","params":[]}`,
		`{"jsonrpc":"2.0","id":1,"method":"m","params":["0x1",true]}`,
	}
	for i, w := range want {
		if bodies[i] != w {
			t.Errorf("request %d body = %s, want %s", i, bodies[i], w)
		}
	}
}
