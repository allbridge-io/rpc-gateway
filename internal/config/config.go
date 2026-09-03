// Package config describes the TOML configuration of the RPC gateway and
// knows how to load it from files and environment variables.
//
// One configuration file describes every chain the gateway serves. Each chain
// is exposed on its own URL path prefix (for example "/SOL") and has its own
// ordered list of upstream RPC targets.
package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

// ChainType tells the gateway which JSON-RPC dialect a chain speaks. It changes
// how health checks are performed and whether WebSocket proxying is enabled.
type ChainType string

const (
	ChainTypeEVM    ChainType = "evm"
	ChainTypeSolana ChainType = "solana"
)

// Config is the root of the TOML document.
type Config struct {
	Server       Server           `toml:"server"`
	HealthChecks HealthChecks     `toml:"healthchecks"`
	Exceptions   []Exception      `toml:"exceptions" validate:"omitempty,dive"`
	Chains       map[string]Chain `toml:"chains" validate:"required,min=1,dive"`
}

// Server holds settings of the single HTTP listener.
type Server struct {
	// Port to listen on. The PORT environment variable overrides it (Render sets PORT).
	Port uint `toml:"port" default:"3000" validate:"gt=0,lte=65535"`
	// UpstreamTimeout is how long the gateway waits for an RPC target to start
	// responding before it treats the request as failed and tries another target.
	UpstreamTimeout time.Duration `toml:"upstream_timeout" default:"5s" validate:"gt=0"`
	// ReadTimeout / WriteTimeout bound a single client connection.
	ReadTimeout  time.Duration `toml:"read_timeout" default:"15s" validate:"gt=0"`
	WriteTimeout time.Duration `toml:"write_timeout" default:"30s" validate:"gt=0"`
	// ShutdownTimeout is how long in-flight requests may finish after SIGTERM.
	ShutdownTimeout time.Duration `toml:"shutdown_timeout" default:"10s" validate:"gt=0"`
}

// HealthChecks holds the defaults of the background health checker. A chain may
// override MaxBlockLag.
type HealthChecks struct {
	// Interval between two checks of the same target.
	Interval time.Duration `toml:"interval" default:"5s" validate:"gt=0"`
	// Timeout of a single check call.
	Timeout time.Duration `toml:"timeout" default:"3s" validate:"gt=0"`
	// FailureThreshold is how many consecutive failed checks mark a target unhealthy.
	FailureThreshold uint `toml:"failure_threshold" default:"2" validate:"gte=1"`
	// SuccessThreshold is how many consecutive successful checks mark it healthy again.
	SuccessThreshold uint `toml:"success_threshold" default:"1" validate:"gte=1"`
	// MaxBlockLag marks a target unhealthy when its block (slot) number is behind
	// the best target of the same chain by more than this many blocks.
	// Set max_block_lag = 0 on a chain to disable the check for that chain.
	MaxBlockLag uint64 `toml:"max_block_lag" default:"20"`
}

// Exception describes a text fragment in an RPC response body that must be
// treated as a failure of the target, so the request is retried elsewhere.
type Exception struct {
	Match   string `toml:"match" validate:"required,min=1"`
	Message string `toml:"message"`
}

// Chain is one blockchain network served by the gateway.
type Chain struct {
	Type ChainType `toml:"type" validate:"required,oneof=evm solana"`
	// ChainID is the value eth_chainId is expected to return (EVM only, hex like "0xaa36a7").
	// Optional; used by testnet checks and startup sanity checks.
	ChainID string `toml:"chain_id" validate:"omitempty,hexadecimal_prefixed"`
	// MaxBlockLag overrides HealthChecks.MaxBlockLag for this chain. 0 disables the check.
	MaxBlockLag *uint64     `toml:"max_block_lag"`
	Exceptions  []Exception `toml:"exceptions" validate:"omitempty,dive"`
	Targets     []Target    `toml:"targets" validate:"required,min=1,dive"`
}

// Target is one upstream RPC provider of a chain.
type Target struct {
	Name    string `toml:"name" validate:"required,min=1"`
	HTTPURL string `toml:"http_url" validate:"required,http_url"`
	// WSURL is the WebSocket endpoint (Solana). When empty, HTTPURL is used with ws(s) scheme.
	WSURL string `toml:"ws_url" validate:"omitempty,ws_url"`
	// Compression tells that the target accepts gzip-compressed request bodies as-is.
	Compression       bool `toml:"compression"`
	DisableKeepAlives bool `toml:"disable_keep_alives"`
	// Disabled excludes the target from routing without removing it from the file.
	Disabled bool `toml:"disabled"`
}

var chainKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ChainKeys returns chain keys in a stable (sorted) order.
func (c *Config) ChainKeys() []string {
	keys := make([]string, 0, len(c.Chains))
	for k := range c.Chains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ExceptionsFor returns global exceptions followed by chain-specific ones.
func (c *Config) ExceptionsFor(chainKey string) []Exception {
	chain, ok := c.Chains[chainKey]
	if !ok {
		return append([]Exception(nil), c.Exceptions...)
	}
	out := make([]Exception, 0, len(c.Exceptions)+len(chain.Exceptions))
	out = append(out, c.Exceptions...)
	out = append(out, chain.Exceptions...)
	return out
}

// MaxBlockLagFor returns the effective block lag limit for a chain (0 = disabled).
func (c *Config) MaxBlockLagFor(chainKey string) uint64 {
	if chain, ok := c.Chains[chainKey]; ok && chain.MaxBlockLag != nil {
		return *chain.MaxBlockLag
	}
	return c.HealthChecks.MaxBlockLag
}

// newValidator registers the custom tags used in struct definitions above.
func newValidator() *validator.Validate {
	v := validator.New()
	_ = v.RegisterValidation("http_url", func(fl validator.FieldLevel) bool {
		return hasScheme(fl.Field().String(), "http", "https")
	})
	_ = v.RegisterValidation("ws_url", func(fl validator.FieldLevel) bool {
		return hasScheme(fl.Field().String(), "ws", "wss")
	})
	_ = v.RegisterValidation("hexadecimal_prefixed", func(fl validator.FieldLevel) bool {
		s := fl.Field().String()
		if !strings.HasPrefix(s, "0x") || len(s) < 3 {
			return false
		}
		for _, r := range s[2:] {
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
		return true
	})
	return v
}

func hasScheme(raw string, schemes ...string) bool {
	lower := strings.ToLower(raw)
	for _, s := range schemes {
		if strings.HasPrefix(lower, s+"://") && len(lower) > len(s)+3 {
			return true
		}
	}
	return false
}

// validateSemantics checks the rules that cannot be expressed with struct tags:
// chain key format, uniqueness of chain keys and target names (case-insensitive).
func validateSemantics(c *Config) error {
	seenChains := map[string]string{}
	for _, key := range c.ChainKeys() {
		if !chainKeyPattern.MatchString(key) {
			return fmt.Errorf("chain key %q is invalid: use letters, digits, '_' or '-' (it becomes the URL path)", key)
		}
		lower := strings.ToLower(key)
		if prev, dup := seenChains[lower]; dup {
			return fmt.Errorf("chain keys %q and %q differ only by case; URL routing is case-insensitive", prev, key)
		}
		seenChains[lower] = key

		seenTargets := map[string]string{}
		for _, t := range c.Chains[key].Targets {
			tl := strings.ToLower(t.Name)
			if prev, dup := seenTargets[tl]; dup {
				return fmt.Errorf("chain %q: target names %q and %q are duplicates", key, prev, t.Name)
			}
			seenTargets[tl] = t.Name
		}
	}
	return nil
}
