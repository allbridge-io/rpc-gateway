package gateway

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/proxy"
)

// Status is the body of GET /status.
type Status struct {
	Chains map[string]ChainStatus `json:"chains"`
}

// ChainStatus is one chain inside Status.
type ChainStatus struct {
	Type            config.ChainType     `json:"type"`
	RoutableTargets int                  `json:"routableTargets"`
	Targets         []proxy.TargetStatus `json:"targets"`
}

// Status returns a snapshot of every chain and target.
func (g *Gateway) Status() Status {
	s := Status{Chains: map[string]ChainStatus{}}
	for _, key := range g.order {
		c := g.Chain(key)
		targets := c.Manager.Status()
		routable := 0
		for _, t := range targets {
			if t.Routable {
				routable++
			}
		}
		s.Chains[key] = ChainStatus{Type: c.Type, RoutableTargets: routable, Targets: targets}
	}
	return s
}

// Routes:
//
//	GET  /healthz          liveness: the process is up (never depends on upstreams)
//	GET  /status           JSON snapshot of chains and targets
//	POST /{chain}          JSON-RPC request for the chain (key is case-insensitive)
//	GET  /{chain}          WebSocket upgrade for the chain
func (g *Gateway) newRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"healthy": true})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, g.Status())
	})
	mux.HandleFunc("/", g.serveChain)
	return g.recover(g.logRequests(mux))
}

func (g *Gateway) serveChain(w http.ResponseWriter, r *http.Request) {
	key := strings.Trim(r.URL.Path, "/")
	if key == "" || strings.Contains(key, "/") {
		writeJSONError(w, http.StatusNotFound, "unknown route; use /{chain}, /status or /healthz")
		return
	}
	c := g.Chain(key)
	if c == nil {
		writeJSONError(w, http.StatusNotFound, "unknown chain "+key+"; configured: "+strings.Join(g.order, ", "))
		return
	}
	isUpgrade := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	switch {
	case r.Method == http.MethodPost, isUpgrade && r.Method == http.MethodGet:
		c.Proxy.ServeHTTP(w, r)
	case r.Method == http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, OPTIONS")
		writeJSONError(w, http.StatusMethodNotAllowed, "use POST for JSON-RPC or a WebSocket upgrade")
	}
}

func (g *Gateway) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		g.log.Debug("request",
			zap.String("method", r.Method), zap.String("path", r.URL.Path),
			zap.Int("status", sw.status), zap.Duration("duration", time.Since(start)),
			zap.String("provider", sw.Header().Get("X-Rpc-Provider")))
	})
}

func (g *Gateway) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec) // the standard way to abort a hijacked/streamed response
				}
				g.log.Error("panic in handler", zap.Any("panic", rec), zap.ByteString("stack", debug.Stack()))
				writeJSONError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter remembers the status code for logging. It deliberately does not
// implement http.Hijacker itself: see Unwrap, used by http.ResponseController.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets httputil.ReverseProxy reach the underlying writer for WebSocket hijacking.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error":   map[string]any{"code": -32000, "message": message},
	})
}
