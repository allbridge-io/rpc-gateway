// Package fakenode is a programmable stand-in for an RPC node, used by tests
// to simulate every way a real provider can misbehave: errors, slowness,
// dropped connections, garbage responses, lagging blocks.
//
// What a node answers when it behaves comes from the simulation registered for
// its chain type in sim_<type>.go (see sim.go); the failure knobs of Behavior
// work the same for every type.
package fakenode

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// Behavior tells the node how to answer. The zero value is a healthy node at block 0.
type Behavior struct {
	// Block is the number the health call of the node's chain type reports:
	// block, slot, ledger, checkpoint or round.
	Block uint64
	// ChainID is returned by eth_chainId (default "0x1"), on EVM and Tron's /jsonrpc.
	ChainID string
	// Latency delays every response.
	Latency time.Duration
	// HTTPStatus, when not 0/200, is returned with a plain-text body.
	HTTPStatus int
	// RawBody, when set, is written verbatim (useful for invalid JSON). With
	// HTTPStatus it produces an API-shaped error body with a real status code.
	RawBody string
	// RPCError, when set, is returned as a JSON-RPC error for every method.
	RPCError string
	// Drop closes the TCP connection without answering.
	Drop bool
	// Hang never answers; the handler waits for the request to be cancelled.
	Hang bool
	// GzipResponse compresses the response body (Content-Encoding: gzip).
	GzipResponse bool
	// TronError makes a Tron node answer 200 with {"Error": "..."} (how Tron
	// reports most failures); see sim_tron.go.
	TronError string
	// TronResultCode makes a Tron node answer 200 with {"result": {"code": "...", "message": "<hex>"}}.
	TronResultCode string
}

// Call is one recorded request.
type Call struct {
	Method     string // JSON-RPC method, "" for non JSON-RPC bodies
	HTTPMethod string
	Path       string
	Query      string
	Body       string
	Header     http.Header
}

// Node is a fake RPC node bound to a local test server.
type Node struct {
	Name string
	Type config.ChainType

	srv *httptest.Server
	mu  sync.Mutex
	b   Behavior
	log []Call
}

// New starts a fake node. It is closed automatically when the test ends.
func New(t testing.TB, name string, typ config.ChainType) *Node {
	t.Helper()
	n := &Node{Name: name, Type: typ}
	n.srv = httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(n.srv.Close)
	return n
}

// URL is the HTTP endpoint of the node.
func (n *Node) URL() string { return n.srv.URL }

// Target returns a config.Target pointing at this node.
func (n *Node) Target() config.Target {
	return config.Target{Name: n.Name, HTTPURL: n.srv.URL}
}

// Set replaces the behavior atomically.
func (n *Node) Set(b Behavior) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.b = b
}

// Update modifies the behavior in place.
func (n *Node) Update(fn func(*Behavior)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fn(&n.b)
}

// Calls returns every recorded request.
func (n *Node) Calls() []Call {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Call(nil), n.log...)
}

// CallCount returns how many requests used the given JSON-RPC method ("" = any).
func (n *Node) CallCount(method string) int {
	count := 0
	for _, c := range n.Calls() {
		if method == "" || c.Method == method {
			count++
		}
	}
	return count
}

// ResetCalls forgets recorded requests.
func (n *Node) ResetCalls() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.log = nil
}

func (n *Node) serve(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	b := n.b
	n.mu.Unlock()

	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(raw, &req)

	n.mu.Lock()
	n.log = append(n.log, Call{
		Method: req.Method, HTTPMethod: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Body: string(raw), Header: r.Header.Clone(),
	})
	n.mu.Unlock()

	if b.Latency > 0 {
		select {
		case <-time.After(b.Latency):
		case <-r.Context().Done():
			return
		}
	}
	if b.Hang {
		<-r.Context().Done()
		return
	}
	if b.Drop {
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("fakenode: response writer cannot hijack")
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0) // RST instead of FIN: looks like a crash
			}
			conn.Close()
		}
		return
	}

	w.Header().Set("X-Fake-Node", n.Name)
	if b.HTTPStatus != 0 && b.HTTPStatus != http.StatusOK {
		if b.RawBody != "" {
			// A provider-shaped error body with a real status code, the way
			// REST APIs report client errors and rate limits.
			n.write(w, r, b, &Response{Status: b.HTTPStatus, Body: b.RawBody})
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(b.HTTPStatus)
		fmt.Fprintf(w, "%s: upstream error %d", n.Name, b.HTTPStatus)
		return
	}

	n.write(w, r, b, n.payload(r, b, req.Method, string(raw)))
}

// payload picks what the node answers: the generic knobs first, then the
// simulation of its chain type, then the generic echo.
func (n *Node) payload(r *http.Request, b Behavior, rpcMethod, body string) any {
	switch {
	case b.RawBody != "":
		return &Response{Body: b.RawBody}
	case b.RPCError != "":
		return RPCErrorBody(body, -32000, b.RPCError)
	}
	if sim, ok := simFor(n.Type); ok {
		if p, handled := sim(n, r, b, rpcMethod, body); handled {
			return p
		}
	}
	// Identify the node in the result so tests can see who answered.
	if rpcMethod == "" && n.Type.PassThroughPath() {
		return n.Echo(r, body)
	}
	return RPCResult(body, map[string]any{"node": n.Name, "method": rpcMethod})
}

// write sends the payload, compressing it when the behavior and the client's
// Accept-Encoding ask for it.
func (n *Node) write(w http.ResponseWriter, r *http.Request, b Behavior, payload any) {
	status := http.StatusOK
	contentType := "application/json"
	var data []byte
	if resp, ok := payload.(*Response); ok {
		if resp.Status != 0 {
			status = resp.Status
		}
		if resp.ContentType != "" {
			contentType = resp.ContentType
		}
		data = []byte(resp.Body)
	} else {
		data = mustJSON(payload)
	}

	w.Header().Set("Content-Type", contentType)
	if b.GzipResponse && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(data)
		_ = zw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		data = buf.Bytes()
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
