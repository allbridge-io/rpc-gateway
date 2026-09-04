package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalTOML = `
[chains.SPL]
type = "evm"
[[chains.SPL.targets]]
name = "One"
http_url = "https://one.example"
`

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func noEnv(string) (string, bool) { return "", false }

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func mustLoad(t *testing.T, opts LoadOptions) *Config {
	t.Helper()
	if opts.LookupEnv == nil {
		opts.LookupEnv = noEnv
	}
	cfg, err := Load(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return cfg
}

func TestLoad_DefaultsApplied(t *testing.T) {
	cfg := mustLoad(t, LoadOptions{ConfigPath: writeFile(t, "c.toml", minimalTOML)})

	if cfg.Server.Port != 3000 {
		t.Errorf("port: got %d want 3000", cfg.Server.Port)
	}
	if cfg.Server.UpstreamTimeout != 5*time.Second {
		t.Errorf("upstream_timeout: got %v", cfg.Server.UpstreamTimeout)
	}
	if cfg.HealthChecks.Interval != 5*time.Second || cfg.HealthChecks.Timeout != 3*time.Second {
		t.Errorf("healthchecks defaults: %+v", cfg.HealthChecks)
	}
	if cfg.HealthChecks.FailureThreshold != 2 || cfg.HealthChecks.SuccessThreshold != 1 {
		t.Errorf("thresholds: %+v", cfg.HealthChecks)
	}
	if cfg.HealthChecks.MaxBlockLag != 20 {
		t.Errorf("max_block_lag default: got %d", cfg.HealthChecks.MaxBlockLag)
	}
	if got := cfg.HealthChecks.TaintDurationOrDefault(); got != DefaultTaintDuration {
		t.Errorf("taint_duration default: got %v", got)
	}
	chain := cfg.Chains["SPL"]
	if chain.Type != ChainTypeEVM || len(chain.Targets) != 1 || chain.Targets[0].Name != "One" {
		t.Errorf("chain: %+v", chain)
	}
	if got := cfg.MaxBlockLagFor("SPL"); got != 20 {
		t.Errorf("MaxBlockLagFor: got %d", got)
	}
}

func TestLoad_FullConfig(t *testing.T) {
	full := `
[server]
port = 8080
upstream_timeout = "2s"
read_timeout = "10s"
write_timeout = "20s"
shutdown_timeout = "3s"

[healthchecks]
interval = "1s"
timeout = "500ms"
failure_threshold = 3
success_threshold = 2
max_block_lag = 10
taint_duration = "0s"

[[exceptions]]
match = "socket hang up"

[chains.SOL]
type = "solana"
max_block_lag = 0
[[chains.SOL.exceptions]]
match = "Blockhash not found"
message = "Solana: Blockhash not found"
[[chains.SOL.targets]]
name = "Helius"
http_url = "https://sol.example/?api-key=1"
ws_url = "wss://sol.example/?api-key=1"
[[chains.SOL.targets]]
name = "Public"
http_url = "https://api.devnet.solana.com"
disabled = true

[chains.SPL]
type = "evm"
chain_id = "0xaa36a7"
max_block_lag = 5
[[chains.SPL.targets]]
name = "Alchemy"
http_url = "https://eth.example"
compression = true
disable_keep_alives = true
`
	cfg := mustLoad(t, LoadOptions{ConfigPath: writeFile(t, "c.toml", full)})

	if cfg.Server.Port != 8080 || cfg.Server.UpstreamTimeout != 2*time.Second || cfg.Server.ShutdownTimeout != 3*time.Second {
		t.Errorf("server: %+v", cfg.Server)
	}
	if cfg.HealthChecks.Timeout != 500*time.Millisecond || cfg.HealthChecks.FailureThreshold != 3 {
		t.Errorf("healthchecks: %+v", cfg.HealthChecks)
	}
	if got := cfg.HealthChecks.TaintDurationOrDefault(); got != 0 {
		t.Errorf("taint_duration = 0s must disable tainting, got %v", got)
	}
	if keys := cfg.ChainKeys(); strings.Join(keys, ",") != "SOL,SPL" {
		t.Errorf("ChainKeys: %v", keys)
	}
	sol := cfg.Chains["SOL"]
	if sol.Type != ChainTypeSolana || sol.Targets[0].WSURL != "wss://sol.example/?api-key=1" || !sol.Targets[1].Disabled {
		t.Errorf("SOL: %+v", sol)
	}
	if got := cfg.MaxBlockLagFor("SOL"); got != 0 {
		t.Errorf("SOL lag should be disabled, got %d", got)
	}
	if got := cfg.MaxBlockLagFor("SPL"); got != 5 {
		t.Errorf("SPL lag: got %d", got)
	}
	if got := cfg.MaxBlockLagFor("UNKNOWN"); got != 10 {
		t.Errorf("unknown chain lag should fall back to global 10, got %d", got)
	}
	spl := cfg.Chains["SPL"]
	if spl.ChainID != "0xaa36a7" || !spl.Targets[0].Compression || !spl.Targets[0].DisableKeepAlives {
		t.Errorf("SPL: %+v", spl)
	}
	exc := cfg.ExceptionsFor("SOL")
	if len(exc) != 2 || exc[0].Match != "socket hang up" || exc[1].Message != "Solana: Blockhash not found" {
		t.Errorf("ExceptionsFor(SOL): %+v", exc)
	}
	if exc := cfg.ExceptionsFor("SPL"); len(exc) != 1 {
		t.Errorf("ExceptionsFor(SPL): %+v", exc)
	}
}

func TestLoad_TronChainWithHeaders(t *testing.T) {
	text := `
[chains.TRX]
type = "tron"
[[chains.TRX.targets]]
name = "TronGrid"
http_url = "https://api.trongrid.io"
headers = { "TRON-PRO-API-KEY" = "${TRONGRID_KEY}", "X-Team" = "allbridge" }
`
	cfg := mustLoad(t, LoadOptions{ConfigPath: writeFile(t, "c.toml", text), LookupEnv: envOf(map[string]string{"TRONGRID_KEY": "k"})})
	trx := cfg.Chains["TRX"]
	if trx.Type != ChainTypeTron || !trx.Type.PassThroughPath() {
		t.Errorf("tron type: %+v", trx)
	}
	if got := trx.Targets[0].Headers; got["TRON-PRO-API-KEY"] != "k" || got["X-Team"] != "allbridge" {
		t.Errorf("headers with placeholders: %v", got)
	}
	if ChainTypeEVM.PassThroughPath() || ChainTypeSolana.PassThroughPath() {
		t.Error("only tron passes the sub-path through")
	}

	_, err := Load(LoadOptions{ConfigPath: writeFile(t, "bad.toml", strings.Replace(text, `"X-Team" = "allbridge"`, `"X-Team" = ""`, 1)),
		LookupEnv: envOf(map[string]string{"TRONGRID_KEY": "k"})})
	if err == nil || !strings.Contains(err.Error(), "Headers") {
		t.Errorf("empty header value must be rejected, got %v", err)
	}
}

func TestLoad_EnvPlaceholders(t *testing.T) {
	text := `
[chains.SPL]
type = "evm"
[[chains.SPL.targets]]
name = "Alchemy"
http_url = "https://eth.example/v2/${ALCHEMY_KEY}"
`
	path := writeFile(t, "c.toml", text)

	cfg := mustLoad(t, LoadOptions{ConfigPath: path, LookupEnv: envOf(map[string]string{"ALCHEMY_KEY": "k123"})})
	if got := cfg.Chains["SPL"].Targets[0].HTTPURL; got != "https://eth.example/v2/k123" {
		t.Errorf("placeholder not expanded: %s", got)
	}

	_, err := Load(LoadOptions{ConfigPath: path, LookupEnv: noEnv})
	if err == nil || !strings.Contains(err.Error(), "ALCHEMY_KEY") {
		t.Errorf("expected error naming the missing variable, got: %v", err)
	}
}

func TestLoad_SecretAndOverridePrecedence(t *testing.T) {
	base := `
[server]
port = 3000
[chains.SPL]
type = "evm"
[[chains.SPL.targets]]
name = "One"
http_url = "https://base.example"
`
	secret := `
[server]
port = 4000
[chains.SPL]
type = "evm"
[[chains.SPL.targets]]
name = "One"
http_url = "https://secret.example/${KEY}"

[chains.SOL]
type = "solana"
[[chains.SOL.targets]]
name = "Sol"
http_url = "https://sol.example"
`
	override := `
[server]
port = 5000
`
	cfg := mustLoad(t, LoadOptions{
		ConfigPath:       writeFile(t, "base.toml", base),
		SecretConfigPath: writeFile(t, "secret.toml", secret),
		OverrideTOML:     override,
		LookupEnv:        envOf(map[string]string{"KEY": "s3cr3t"}),
	})
	if cfg.Server.Port != 5000 {
		t.Errorf("override should win: port %d", cfg.Server.Port)
	}
	if got := cfg.Chains["SPL"].Targets[0].HTTPURL; got != "https://secret.example/s3cr3t" {
		t.Errorf("secret should replace base target: %s", got)
	}
	if _, ok := cfg.Chains["SOL"]; !ok {
		t.Errorf("chain added by secret file is missing")
	}
}

func TestLoad_PortOptionOverridesEverything(t *testing.T) {
	cfg := mustLoad(t, LoadOptions{
		ConfigPath:   writeFile(t, "c.toml", "[server]\nport = 3000\n"+minimalTOML),
		OverrideTOML: "[server]\nport = 4000\n",
		Port:         10000,
	})
	if cfg.Server.Port != 10000 {
		t.Errorf("Port option should win: %d", cfg.Server.Port)
	}
}

func TestLoad_SectionExtraction(t *testing.T) {
	shared := `
[common.chains.SPL]
node_url = "https://this-belongs-to-another-service"

[rpc_gateway.server]
port = 7000
[rpc_gateway.chains.SPL]
type = "evm"
[[rpc_gateway.chains.SPL.targets]]
name = "One"
http_url = "https://one.example"
`
	secretWithoutSection := `
[common.telegram]
chat_id = "1"
`
	cfg := mustLoad(t, LoadOptions{
		ConfigPath:       writeFile(t, "shared.toml", shared),
		SecretConfigPath: writeFile(t, "secret.toml", secretWithoutSection),
		Section:          "rpc_gateway",
	})
	if cfg.Server.Port != 7000 || cfg.Chains["SPL"].Targets[0].HTTPURL != "https://one.example" {
		t.Errorf("section not extracted correctly: %+v", cfg)
	}
}

func TestLoadFromEnv(t *testing.T) {
	path := writeFile(t, "c.toml", minimalTOML)
	t.Setenv(EnvConfigPath, path)
	t.Setenv(EnvPort, "9999")
	t.Setenv(EnvConfigOverride, "[healthchecks]\ninterval = \"7s\"\n")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if cfg.Server.Port != 9999 || cfg.HealthChecks.Interval != 7*time.Second {
		t.Errorf("env not applied: %+v %+v", cfg.Server, cfg.HealthChecks)
	}

	t.Setenv(EnvPort, "not-a-number")
	if _, err := LoadFromEnv(); err == nil || !strings.Contains(err.Error(), EnvPort) {
		t.Errorf("expected PORT error, got %v", err)
	}

	t.Setenv(EnvPort, "")
	t.Setenv(EnvConfigPath, "")
	if _, err := LoadFromEnv(); err == nil || !strings.Contains(err.Error(), EnvConfigPath) {
		t.Errorf("expected missing CONFIG_TOML_PATH error, got %v", err)
	}
}

func TestLoad_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{"no chains", "[server]\nport = 1\n", "Chains"},
		{"chain without targets", "[chains.SPL]\ntype = \"evm\"\n", "Targets"},
		{"unknown chain type", strings.Replace(minimalTOML, `"evm"`, `"cosmos"`, 1), "oneof"},
		{"missing type", strings.Replace(minimalTOML, "type = \"evm\"\n", "", 1), "Type"},
		{"target without name", strings.Replace(minimalTOML, "name = \"One\"\n", "", 1), "Name"},
		{"http_url with wrong scheme", strings.Replace(minimalTOML, "https://one.example", "ftp://one.example", 1), "http_url"},
		{"http_url not a url", strings.Replace(minimalTOML, "https://one.example", "one.example", 1), "http_url"},
		{"ws_url with http scheme", minimalTOML + "ws_url = \"https://one.example\"\n", "ws_url"},
		{"bad chain_id", "[chains.SPL]\ntype = \"evm\"\nchain_id = \"11155111\"\n[[chains.SPL.targets]]\nname = \"One\"\nhttp_url = \"https://one.example\"\n", "hexadecimal_prefixed"},
		{"duplicate target names ignoring case", minimalTOML + "[[chains.SPL.targets]]\nname = \"one\"\nhttp_url = \"https://two.example\"\n", "duplicates"},
		{"chain keys differing by case", minimalTOML + "[chains.spl]\ntype = \"evm\"\n[[chains.spl.targets]]\nname = \"X\"\nhttp_url = \"https://x.example\"\n", "differ only by case"},
		{"chain key with slash", strings.ReplaceAll(minimalTOML, "SPL", `"a/b"`), "invalid"},
		{"chain key with space", strings.ReplaceAll(minimalTOML, "SPL", `"a b"`), "invalid"},
		{"negative interval", "[healthchecks]\ninterval = \"-1s\"\n" + minimalTOML, "Interval"},
		{"port out of range", "[server]\nport = 70000\n" + minimalTOML, "Port"},
		{"empty exception match", "[[exceptions]]\nmatch = \"\"\n" + minimalTOML, "Match"},
		{"unknown key (typo)", "[server]\nprot = 3000\n" + minimalTOML, "unknown keys"},
		{"unknown key in target", minimalTOML + "wss_url = \"wss://x\"\n", "unknown keys"},
		{"malformed toml", "[chains.SPL\ntype = evm", "TOML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(LoadOptions{ConfigPath: writeFile(t, "c.toml", tt.toml), LookupEnv: noEnv})
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// Explicit zero values are replaced by defaults before validation (behaviour of
// the go-kit TOML builder, same as in allbridge-next-info-server). Documented here
// so nobody expects "0" to mean "disabled" for these fields.
func TestLoad_ExplicitZeroUsesDefault(t *testing.T) {
	cfg := mustLoad(t, LoadOptions{ConfigPath: writeFile(t, "c.toml",
		"[server]\nupstream_timeout = \"0s\"\n[healthchecks]\nfailure_threshold = 0\nmax_block_lag = 0\n"+minimalTOML)})
	if cfg.Server.UpstreamTimeout != 5*time.Second {
		t.Errorf("upstream_timeout: got %v want default 5s", cfg.Server.UpstreamTimeout)
	}
	if cfg.HealthChecks.FailureThreshold != 2 {
		t.Errorf("failure_threshold: got %d want default 2", cfg.HealthChecks.FailureThreshold)
	}
	if cfg.HealthChecks.MaxBlockLag != 20 {
		t.Errorf("global max_block_lag = 0 becomes default 20; got %d (disable it per chain instead)", cfg.HealthChecks.MaxBlockLag)
	}
}

func TestLoad_PlaceholderInCommentIsIgnored(t *testing.T) {
	cfg := mustLoad(t, LoadOptions{ConfigPath: writeFile(t, "c.toml",
		"# reference secrets as ${NAME}\n"+minimalTOML+"# ${ANOTHER}\n")})
	if cfg.Chains["SPL"].Targets[0].HTTPURL != "https://one.example" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoad_FileErrors(t *testing.T) {
	if _, err := Load(LoadOptions{}); err == nil {
		t.Error("empty ConfigPath must fail")
	}
	if _, err := Load(LoadOptions{ConfigPath: "/nonexistent/c.toml", LookupEnv: noEnv}); err == nil {
		t.Error("missing config file must fail")
	}
	base := writeFile(t, "c.toml", minimalTOML)
	if _, err := Load(LoadOptions{ConfigPath: base, SecretConfigPath: "/nonexistent/s.toml", LookupEnv: noEnv}); err == nil {
		t.Error("missing secret file must fail when it is configured")
	}
}

// TestExampleConfigLoads keeps config.example.toml in the repo root valid.
func TestExampleConfigLoads(t *testing.T) {
	cfg := mustLoad(t, LoadOptions{
		ConfigPath: filepath.Join("..", "..", "config.example.toml"),
		LookupEnv:  envOf(map[string]string{"ALCHEMY_KEY": "example"}),
	})
	for _, key := range []string{"SPL", "ARB", "TRX", "SOL"} {
		if _, ok := cfg.Chains[key]; !ok {
			t.Errorf("example config lacks chain %s", key)
		}
	}
	if cfg.Chains["SOL"].Type != ChainTypeSolana || cfg.Chains["TRX"].Type != ChainTypeTron {
		t.Errorf("example chain types: SOL=%s TRX=%s", cfg.Chains["SOL"].Type, cfg.Chains["TRX"].Type)
	}
}
