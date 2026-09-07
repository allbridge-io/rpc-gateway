package fakenode

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// SimFunc simulates the API of one chain type. It receives the node, the
// request, the current behavior, the JSON-RPC method of the body ("" when the
// body is not JSON-RPC) and the raw body. It returns the payload to answer with
// and whether it handled the request; an unhandled request falls back to the
// generic behaviour (a JSON-RPC response echoing the node name and method, or,
// for pass-through types, an echo of method, path, query and body).
//
// The payload is marshalled to JSON, unless it is a *Response, which is written
// verbatim with its own status code.
//
// A simulation must stay honest about how the real API answers: the point of
// these fakes is that a health check or a proxy test fails here for the same
// reason it would fail against a real provider. Type-specific knobs belong to
// the sim's own file; prefer Behavior.RawBody (optionally with HTTPStatus) over
// adding fields to Behavior, which every chain type shares.
type SimFunc func(n *Node, r *http.Request, b Behavior, rpcMethod string, body string) (payload any, handled bool)

// Response is a payload that also sets the HTTP status and content type, for
// APIs that report errors with a status code and a body of their own shape.
type Response struct {
	Status      int    // 0 means 200
	Body        string // written verbatim
	ContentType string // defaults to application/json
}

var (
	simMu sync.RWMutex
	sims  = map[config.ChainType]SimFunc{}
)

// RegisterSim registers the simulation of a chain type. It panics on a
// duplicate registration, which is always a programming error.
func RegisterSim(typ config.ChainType, f SimFunc) {
	if f == nil {
		panic("fakenode: RegisterSim with a nil function")
	}
	simMu.Lock()
	defer simMu.Unlock()
	if _, dup := sims[typ]; dup {
		panic("fakenode: chain type " + string(typ) + " is simulated twice")
	}
	sims[typ] = f
}

func simFor(typ config.ChainType) (SimFunc, bool) {
	simMu.RLock()
	defer simMu.RUnlock()
	f, ok := sims[typ]
	return f, ok
}

// RPCResult builds a JSON-RPC 2.0 response around a result, echoing the id of
// the request body.
func RPCResult(reqBody string, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rpcID(reqBody), "result": result}
}

// RPCErrorBody builds a JSON-RPC 2.0 error response.
func RPCErrorBody(reqBody string, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rpcID(reqBody), "error": map[string]any{"code": code, "message": message}}
}

// Echo is the "who answered and with what" payload used by pass-through types
// for any path they do not simulate.
func (n *Node) Echo(r *http.Request, body string) map[string]any {
	return map[string]any{
		"node": n.Name, "httpMethod": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery, "body": body,
	}
}

// rpcID returns the id of a JSON-RPC request body, 1 when there is none.
func rpcID(body string) json.RawMessage {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	if len(req.ID) == 0 {
		return json.RawMessage("1")
	}
	return req.ID
}
