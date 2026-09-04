//go:build testnet

package testnet

import (
	"net/http"
	"testing"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// typeChecks are the real-network checks of one chain type. Every type
// registers its own in checks_<type>_test.go, so adding a chain type adds a
// file instead of editing a switch here.
//
// (The registry lives in a _test.go file because it refers to *env and
// *testing.T, which the test files declare.)
type typeChecks struct {
	// Verify runs the detailed subtests of one chain of this type.
	Verify func(t *testing.T, e *env, key string, chain config.Chain)
	// Probe makes one cheap real call through the gateway and returns the
	// response; TestTestnetReroute repeats it until it hits the dead target.
	Probe func(t *testing.T, e *env, key string) *http.Response
}

var checks = map[config.ChainType]typeChecks{}

// registerChecks is called from the init() of every checks_<type>_test.go.
func registerChecks(typ config.ChainType, c typeChecks) {
	if c.Verify == nil || c.Probe == nil {
		panic("testnet: incomplete checks for chain type " + string(typ))
	}
	if _, dup := checks[typ]; dup {
		panic("testnet: chain type " + string(typ) + " registers checks twice")
	}
	checks[typ] = c
}

// checksFor fails the test when a configured chain type has no checks: a type
// nobody verifies against the real network must never pass silently.
func checksFor(t *testing.T, typ config.ChainType) typeChecks {
	t.Helper()
	c, ok := checks[typ]
	if !ok {
		t.Fatalf("no testnet checks registered for chain type %q (add tests/testnet/checks_%s_test.go)", typ, typ)
	}
	return c
}
