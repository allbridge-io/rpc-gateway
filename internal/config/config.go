// Package config describes the TOML configuration of the RPC gateway and
// knows how to load it from files and environment variables.
//
// One configuration file describes every chain the gateway serves. Each chain
// is exposed on its own URL path prefix (for example "/SOL") and has its own
// ordered list of upstream RPC targets.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"

	"github.com/0xProject/rpc-gateway/internal/chaintype"
)

// ChainType tells the gateway which dialect a chain speaks. It changes how
// health checks are performed and whether the client's sub-path is forwarded.
// The valid values are the names registered in internal/chaintype; the
// constants below only spare the rest of the code a string literal.
type ChainType string

const (
	ChainTypeEVM    ChainType = chaintype.EVM
	ChainTypeSolana ChainType = chaintype.Solana
	// ChainTypeTron proxies the Tron HTTP API (/wallet/*, /walletsolidity/*,
	// /v1/*, /jsonrpc): the client's path and query are appended to the target URL.
	ChainTypeTron ChainType = chaintype.Tron
	// ChainTypeSui is a single-endpoint JSON-RPC chain (Sui fullnode).
	ChainTypeSui ChainType = chaintype.Sui
	// ChainTypeSoroban is a single-endpoint JSON-RPC chain (Stellar Soroban RPC).
	ChainTypeSoroban ChainType = chaintype.Soroban
	// ChainTypeHorizon proxies the Stellar Horizon REST API (path pass-through).
	ChainTypeHorizon ChainType = chaintype.Horizon
	// ChainTypeTON proxies toncenter (v3 REST and /api/v2/jsonRPC, path pass-through).
	ChainTypeTON ChainType = chaintype.TON
	// ChainTypeStacks proxies the Stacks Hiro API (path pass-through).
	ChainTypeStacks ChainType = chaintype.Stacks
)

// PassThroughPath tells whether the client's sub-path after /{chain} is
// forwarded to the target (REST APIs such as Tron) or ignored (single JSON-RPC
// endpoint). An unknown type passes nothing through.
func (t ChainType) PassThroughPath() bool {
	spec, ok := chaintype.Lookup(string(t))
	return ok && spec.PassThrough
}

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
	// APIKeys, when non-empty, turns on URL-embedded authentication: every
	// route except /healthz must be prefixed with one of the keys, as in
	// /{key}/{chain}, /{key}/{chain}/{path} and /{key}/status. Several keys
	// let a key be rotated without downtime (add the new one, move the
	// clients, drop the old one). Empty = no authentication (the default).
	APIKeys []string `toml:"api_keys" validate:"omitempty,dive,min=1"`
}

// MinAPIKeyLength is the shortest api_keys entry accepted: the key is the
// only thing between the internet and the providers' quotas.
const MinAPIKeyLength = 16

// AuthEnabled tells whether requests must carry an API key in the URL.
func (s Server) AuthEnabled() bool { return len(s.APIKeys) > 0 }

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
	// Unset = 20. 0 disables the check; a chain may override either way.
	MaxBlockLag *uint64 `toml:"max_block_lag"`
	// TaintDuration is how long a target is excluded from routing after a request
	// to it failed at transport level or with HTTP 5xx / 429 and was retried
	// elsewhere. Unset = 15s. "0s" disables tainting. Exception matches never taint.
	TaintDuration *time.Duration `toml:"taint_duration"`
}

// DefaultTaintDuration applies when healthchecks.taint_duration is not set.
const DefaultTaintDuration = 15 * time.Second

// DefaultMaxBlockLag applies when healthchecks.max_block_lag is not set.
const DefaultMaxBlockLag uint64 = 20

// MaxBlockLagOrDefault returns the effective global block lag limit (0 = disabled).
func (h HealthChecks) MaxBlockLagOrDefault() uint64 {
	if h.MaxBlockLag == nil {
		return DefaultMaxBlockLag
	}
	return *h.MaxBlockLag
}

// TaintDurationOrDefault returns the effective taint duration (0 = disabled).
func (h HealthChecks) TaintDurationOrDefault() time.Duration {
	if h.TaintDuration == nil {
		return DefaultTaintDuration
	}
	if *h.TaintDuration < 0 {
		return 0
	}
	return *h.TaintDuration
}

// Exception describes a text fragment in an RPC response body that must be
// treated as a failure of the target, so the request is retried elsewhere.
type Exception struct {
	Match   string `toml:"match" validate:"required,min=1"`
	Message string `toml:"message"`
}

// Chain is one blockchain network served by the gateway.
type Chain struct {
	Type ChainType `toml:"type" validate:"required,chain_type"`
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
	// Headers are added to every request sent to the target (for example an API
	// key header such as TRON-PRO-API-KEY). They override client headers of the same name.
	Headers map[string]string `toml:"headers" validate:"omitempty,dive,keys,min=1,endkeys,required"`
	// Compression tells that the target accepts gzip-compressed request bodies as-is.
	Compression       bool `toml:"compression"`
	DisableKeepAlives bool `toml:"disable_keep_alives"`
	// Disabled excludes the target from routing without removing it from the file.
	Disabled bool `toml:"disabled"`
}

var chainKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// apiKeyPattern allows the URL-unreserved characters only, so a key is one
// path segment that needs no percent-encoding anywhere (curl, TronWeb, web3 providers).
var apiKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_.~-]+$`)

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
	return c.HealthChecks.MaxBlockLagOrDefault()
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
	_ = v.RegisterValidation("chain_type", func(fl validator.FieldLevel) bool {
		_, ok := chaintype.Lookup(fl.Field().String())
		return ok
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

// describeValidationError appends the list of known chain types when a chain
// declares an unknown one: the tag alone would not say what is allowed.
func describeValidationError(err error) error {
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return err
	}
	for _, fe := range verrs {
		if fe.Tag() == "chain_type" {
			return fmt.Errorf("%w; known chain types: %s", err, strings.Join(chaintype.Names(), ", "))
		}
	}
	return err
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
// API key format, chain key format, uniqueness of chain keys and target names
// (case-insensitive).
func validateSemantics(c *Config) error {
	seenKeys := map[string]struct{}{}
	for i, key := range c.Server.APIKeys {
		// Never echo the key itself: config errors end up in logs.
		if len(key) < MinAPIKeyLength {
			return fmt.Errorf("server.api_keys[%d] is too short: use at least %d characters (openssl rand -hex 32)", i, MinAPIKeyLength)
		}
		if !apiKeyPattern.MatchString(key) {
			return fmt.Errorf("server.api_keys[%d] is invalid: use letters, digits, '-', '_', '.' or '~' (it becomes a URL path segment)", i)
		}
		if _, dup := seenKeys[key]; dup {
			return fmt.Errorf("server.api_keys[%d] repeats an earlier key", i)
		}
		seenKeys[key] = struct{}{}
	}

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
