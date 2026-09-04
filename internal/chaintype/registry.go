// Package chaintype is the registry of the protocol dialects the gateway can
// serve. One chain type ("evm", "solana", "tron", ...) knows two things: how to
// read the head of the chain from a target (the health check) and whether the
// client's sub-path after /{chain} is forwarded to the target or ignored.
//
// Every type lives in its own file and registers itself in init(), so adding a
// type means adding files, not editing shared code. The registry is the single
// source of truth for the rest of the gateway: internal/config validates
// chains.X.type against it, and internal/proxy resolves the health check
// through it instead of switching on the type itself.
package chaintype

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// HeadFunc reports the number of the latest block (slot, ledger, checkpoint,
// round...) of one target. baseURL is the target's http_url as configured and
// headers are its per-target headers, which must be sent with the check so an
// API key is not missing exactly where it is needed most.
type HeadFunc func(ctx context.Context, client *http.Client, baseURL string, headers map[string]string) (uint64, error)

// Spec describes one chain type.
type Spec struct {
	// Name is the value of chains.X.type in the configuration.
	Name string
	// PassThrough tells that the client's sub-path, query and HTTP method are
	// forwarded to the target (REST APIs) instead of being ignored (single
	// JSON-RPC endpoint).
	PassThrough bool
	// Head performs one health check call.
	Head HeadFunc
	// Doc is the one-line description used in the README chain types table.
	Doc string
}

var (
	mu       sync.RWMutex
	registry = map[string]Spec{}
)

// Register adds a chain type. It panics on a duplicate name or an incomplete
// spec: both are programming errors visible at process start.
func Register(s Spec) {
	if s.Name == "" {
		panic("chaintype: Register with an empty name")
	}
	if s.Head == nil {
		panic("chaintype: Register " + s.Name + " without a Head function")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[s.Name]; dup {
		panic(fmt.Sprintf("chaintype: %q is registered twice", s.Name))
	}
	registry[s.Name] = s
}

// Lookup returns the spec of a type name.
func Lookup(name string) (Spec, bool) {
	mu.RLock()
	defer mu.RUnlock()
	s, ok := registry[name]
	return s, ok
}

// Names returns every registered type name, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
