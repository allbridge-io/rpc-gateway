package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// newReverseProxies builds the HTTP and the WebSocket reverse proxy of a target.
// Both forward to the target's own path and query (an API key often lives in
// the query), ignoring the client's path, which is only used to pick the chain.
func newReverseProxies(t config.Target, upstreamTimeout time.Duration) (httpProxy, wsProxy *httputil.ReverseProxy, err error) {
	httpTarget, err := url.Parse(t.HTTPURL)
	if err != nil {
		return nil, nil, fmt.Errorf("target %s: parse http_url: %w", t.Name, err)
	}

	wsRaw := t.WSURL
	if wsRaw == "" {
		wsRaw = t.HTTPURL
	}
	// httputil.ReverseProxy speaks http(s) and handles the Upgrade handshake itself.
	wsRaw = strings.Replace(strings.Replace(wsRaw, "wss://", "https://", 1), "ws://", "http://", 1)
	wsTarget, err := url.Parse(wsRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("target %s: parse ws_url: %w", t.Name, err)
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     t.DisableKeepAlives,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: upstreamTimeout,
	}

	httpProxy = &httputil.ReverseProxy{Director: director(httpTarget), Transport: transport}
	wsProxy = &httputil.ReverseProxy{Director: director(wsTarget), Transport: transport}
	return httpProxy, wsProxy, nil
}

func director(target *url.URL) func(*http.Request) {
	return func(r *http.Request) {
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		r.URL.Path = target.Path
		r.URL.RawPath = target.RawPath
		r.URL.RawQuery = target.RawQuery
		r.Host = target.Host
		if _, ok := r.Header["User-Agent"]; !ok {
			r.Header.Set("User-Agent", "") // do not let net/http add its default UA
		}
	}
}
