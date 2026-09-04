// Package fakenode is a programmable stand-in for an RPC node, used by tests
// to simulate every way a real provider can misbehave: errors, slowness,
// dropped connections, garbage responses, lagging blocks.
package fakenode

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
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
	// Block is the block number (EVM) or slot (Solana) reported by health calls.
	Block uint64
	// ChainID is returned by eth_chainId (default "0x1").
	ChainID string
	// Latency delays every response.
	Latency time.Duration
	// HTTPStatus, when not 0/200, is returned with a plain-text body.
	HTTPStatus int
	// RawBody, when set, is written verbatim (useful for invalid JSON).
	RawBody string
	// RPCError, when set, is returned as a JSON-RPC error for every method.
	RPCError string
	// Drop closes the TCP connection without answering.
	Drop bool
	// Hang never answers; the handler waits for the request to be cancelled.
	Hang bool
	// GzipResponse compresses the response body (Content-Encoding: gzip).
	GzipResponse bool
	// TronError makes a Tron node answer 200 with {"Error": "..."} (how Tron reports most failures).
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
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(raw, &req)
	if len(req.ID) == 0 {
		req.ID = json.RawMessage("1")
	}

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
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(b.HTTPStatus)
		fmt.Fprintf(w, "%s: upstream error %d", n.Name, b.HTTPStatus)
		return
	}

	var payload []byte
	switch {
	case b.RawBody != "":
		payload = []byte(b.RawBody)
	case b.TronError != "":
		payload = mustJSON(map[string]any{"Error": b.TronError})
	case b.TronResultCode != "":
		payload = mustJSON(map[string]any{"result": map[string]any{"code": b.TronResultCode, "message": hex.EncodeToString([]byte(b.TronResultCode))}})
	case b.RPCError != "":
		payload = mustJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": b.RPCError}})
	case n.Type == config.ChainTypeTron && r.URL.Path != "/jsonrpc":
		payload = mustJSON(n.tronResult(r, b, string(raw)))
	default:
		payload = mustJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": n.result(req.Method, b)})
	}

	w.Header().Set("Content-Type", "application/json")
	if b.GzipResponse && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(payload)
		_ = zw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		payload = buf.Bytes()
	}
	_, _ = w.Write(payload)
}

func (n *Node) result(method string, b Behavior) any {
	switch method {
	case "eth_blockNumber":
		return fmt.Sprintf("0x%x", b.Block)
	case "getSlot":
		return b.Block
	case "eth_chainId":
		if b.ChainID == "" {
			return "0x1"
		}
		return b.ChainID
	case "getHealth":
		return "ok"
	default:
		// Identify the node in the result so tests can see who answered.
		return map[string]any{"node": n.Name, "method": method}
	}
}

// tronResult mimics the Tron HTTP API: getnowblock returns a block, anything
// else echoes the request so tests can see what reached the node.
func (n *Node) tronResult(r *http.Request, b Behavior, body string) any {
	switch r.URL.Path {
	case "/wallet/getnowblock", "/walletsolidity/getnowblock":
		return map[string]any{
			"blockID":      fmt.Sprintf("%064x", b.Block),
			"block_header": map[string]any{"raw_data": map[string]any{"number": b.Block, "timestamp": time.Now().UnixMilli()}},
		}
	default:
		return map[string]any{
			"node": n.Name, "httpMethod": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery, "body": body,
		}
	}
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
