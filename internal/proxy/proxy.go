// Package proxy contains the per-chain failover proxy and its health manager.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
)

// DefaultMaxBodyBytes bounds a client request body (JSON-RPC payloads are small).
const DefaultMaxBodyBytes int64 = 8 << 20

// Options configure a Proxy.
type Options struct {
	Chain           string
	Type            config.ChainType
	Targets         []config.Target
	Exceptions      []config.Exception
	UpstreamTimeout time.Duration
	MaxBodyBytes    int64
	Observer        events.Observer
}

type upstream struct {
	cfg       config.Target
	httpProxy *httputil.ReverseProxy
	wsProxy   *httputil.ReverseProxy
}

// Proxy forwards JSON-RPC requests of one chain to a routable target and
// retries on another target when the first one fails.
type Proxy struct {
	opts     Options
	manager  *Manager
	targets  []*upstream
	observer events.Observer
}

// upstreamError is produced by ModifyResponse when the target's reply must be
// treated as a failure. Taint says whether the target should be excluded for a
// while (infrastructure problems) or just this request retried (exceptions).
type upstreamError struct {
	reason string
	taint  bool
	status int
}

func (e *upstreamError) Error() string { return e.reason }

// attempt carries the outcome of one try out of the ReverseProxy callbacks.
type attempt struct {
	err    error
	taint  bool
	status int
}

type attemptKey struct{}

// New builds a Proxy over targets that share the given Manager (same order).
func New(opts Options, manager *Manager) (*Proxy, error) {
	if manager == nil || manager.Len() != len(opts.Targets) {
		return nil, errors.New("proxy: manager must be built from the same targets")
	}
	if opts.Observer == nil {
		opts.Observer = events.Nop{}
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	p := &Proxy{opts: opts, manager: manager, observer: opts.Observer}
	for _, t := range opts.Targets {
		httpProxy, wsProxy, err := newReverseProxies(t, opts.Type, opts.UpstreamTimeout)
		if err != nil {
			return nil, err
		}
		// The body is owned by ReverseProxy, which copies it to the client and closes it.
		httpProxy.ModifyResponse = p.modifyResponse(t) //nolint:bodyclose
		httpProxy.ErrorHandler = errorHandler
		wsProxy.ModifyResponse = p.modifyResponse(t) //nolint:bodyclose
		wsProxy.ErrorHandler = errorHandler
		p.targets = append(p.targets, &upstream{cfg: t, httpProxy: httpProxy, wsProxy: wsProxy})
	}
	return p, nil
}

// Manager returns the health manager shared with this proxy.
func (p *Proxy) Manager() *Manager { return p.manager }

// requestBody holds the client's payload so every retry can replay it.
type requestBody struct {
	raw  []byte
	gzip bool

	once  sync.Once
	plain []byte
	err   error
}

// forTarget returns the bytes to send to a target: decompressed when the client
// sent gzip and the target does not accept it.
func (b *requestBody) forTarget(t config.Target) ([]byte, bool, error) {
	if !b.gzip || t.Compression {
		return b.raw, b.gzip, nil
	}
	b.once.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(b.raw))
		if err != nil {
			b.err = err
			return
		}
		b.plain, b.err = io.ReadAll(zr)
	})
	return b.plain, false, b.err
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	isWS := isWebSocketUpgrade(r)
	var body *requestBody
	if !isWS {
		raw, err := io.ReadAll(io.LimitReader(r.Body, p.opts.MaxBodyBytes+1))
		if err != nil {
			writeRPCError(w, http.StatusBadRequest, "cannot read request body")
			return
		}
		if int64(len(raw)) > p.opts.MaxBodyBytes {
			writeRPCError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		body = &requestBody{raw: raw, gzip: strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip")}
	}
	method := rpcMethod(body)
	if method == "" {
		method = r.Method + " " + r.URL.Path // Tron HTTP API and anything non JSON-RPC
	}

	var excluded []int
	for {
		idx := p.manager.NextRoutable(excluded)
		if idx < 0 {
			p.observer.NoHealthyTargets(p.opts.Chain, len(excluded))
			writeRPCError(w, http.StatusServiceUnavailable, "no healthy upstream for chain "+p.opts.Chain)
			return
		}
		t := p.targets[idx]

		a := &attempt{}
		req := r.Clone(context.WithValue(r.Context(), attemptKey{}, a))
		if !isWS {
			data, compressed, err := body.forTarget(t.cfg)
			if err != nil {
				writeRPCError(w, http.StatusBadRequest, "cannot decompress request body")
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(data))
			req.ContentLength = int64(len(data))
			if !compressed {
				req.Header.Del("Content-Encoding")
			}
		}
		w.Header().Set("X-Rpc-Provider", t.cfg.Name)

		start := time.Now()
		if isWS {
			t.wsProxy.ServeHTTP(w, req)
		} else {
			t.httpProxy.ServeHTTP(w, req)
		}
		p.observer.UpstreamRequest(p.opts.Chain, t.cfg.Name, method, a.status, time.Since(start), a.err)

		if a.err == nil {
			return
		}
		if r.Context().Err() != nil || errors.Is(a.err, context.Canceled) {
			return // the client went away (closed or timed out); retrying is pointless
		}
		p.observer.RequestRerouted(p.opts.Chain, t.cfg.Name, a.err.Error())
		if a.taint {
			p.manager.Taint(idx, a.err.Error())
		}
		excluded = append(excluded, idx)
	}
}

// modifyResponse classifies the target's reply. Returning an error makes the
// ReverseProxy call errorHandler instead of writing anything to the client.
func (p *Proxy) modifyResponse(t config.Target) func(*http.Response) error {
	return func(resp *http.Response) error {
		a := attemptFrom(resp.Request)
		if a != nil {
			a.status = resp.StatusCode
		}
		switch {
		case resp.StatusCode == http.StatusSwitchingProtocols:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests:
			return &upstreamError{reason: "rate limited (429)", taint: true, status: resp.StatusCode}
		case resp.StatusCode >= http.StatusInternalServerError:
			return &upstreamError{reason: fmt.Sprintf("server error (%d)", resp.StatusCode), taint: true, status: resp.StatusCode}
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return &upstreamError{reason: fmt.Sprintf("access denied (%d)", resp.StatusCode), taint: true, status: resp.StatusCode}
		case resp.StatusCode == http.StatusRequestEntityTooLarge:
			return &upstreamError{reason: "request entity too large (413)", taint: false, status: resp.StatusCode}
		}
		if len(p.opts.Exceptions) == 0 {
			return nil
		}
		bodyText, err := peekBody(resp, t)
		if err != nil {
			return &upstreamError{reason: "unreadable response body: " + err.Error(), taint: true, status: resp.StatusCode}
		}
		for _, ex := range p.opts.Exceptions {
			if strings.Contains(bodyText, ex.Match) {
				msg := ex.Message
				if msg == "" {
					msg = ex.Match
				}
				return &upstreamError{reason: "exception: " + msg, taint: false, status: resp.StatusCode}
			}
		}
		return nil
	}
}

// errorHandler records the failure of an attempt; the retry loop decides what to do.
func errorHandler(_ http.ResponseWriter, r *http.Request, err error) {
	a := attemptFrom(r)
	if a == nil {
		return
	}
	a.err = err
	var ue *upstreamError
	switch {
	case errors.As(err, &ue):
		a.taint = ue.taint
		a.status = ue.status
	case errors.Is(err, context.Canceled):
		a.taint = false
	default:
		// dial failure, TLS error, response header timeout, connection reset...
		a.taint = true
	}
}

func attemptFrom(r *http.Request) *attempt {
	if r == nil {
		return nil
	}
	a, _ := r.Context().Value(attemptKey{}).(*attempt)
	return a
}

// peekBody reads the whole response body (decompressing gzip when the target
// does not speak compression natively) and puts it back for the client.
func peekBody(resp *http.Response, t config.Target) (string, error) {
	if resp.Body == nil {
		return "", nil
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return "", err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))

	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") && !t.Compression {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return "", err
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	return string(raw), nil
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// rpcMethod extracts the JSON-RPC method name for logging; "" when unknown.
func rpcMethod(b *requestBody) string {
	if b == nil || b.gzip || len(b.raw) == 0 {
		return ""
	}
	var probe struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(b.raw, &probe); err != nil {
		return ""
	}
	return probe.Method
}

func writeRPCError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error":   map[string]any{"code": -32000, "message": message},
	})
}
