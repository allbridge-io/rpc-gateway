package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/creasty/defaults"
)

// Environment variable names understood by LoadFromEnv.
const (
	EnvConfigPath       = "CONFIG_TOML_PATH"        // required: base TOML file
	EnvSecretConfigPath = "SECRET_CONFIG_TOML_PATH" // optional: merged on top of the base file
	EnvConfigSection    = "CONFIG_TOML_SECTION"     // optional: take only this top-level table of the files
	EnvConfigOverride   = "CONFIG_OVERRIDE_TOML"    // optional: inline TOML merged last
	EnvPort             = "PORT"                    // optional: overrides server.port (set by Render)
)

// LoadOptions describes where the configuration comes from.
//
// Sources are merged in this order, later ones win key by key:
//  1. ConfigPath (required)
//  2. SecretConfigPath (optional)
//  3. OverrideTOML (optional, inline TOML)
//
// Every source may reference environment variables as ${NAME}. A reference to
// an unset variable is an error, so a missing API key never silently becomes "".
type LoadOptions struct {
	ConfigPath       string
	SecretConfigPath string
	OverrideTOML     string
	// Section, when set, means the files are shared with other services and the
	// gateway config lives under this top-level table (for example "rpc_gateway").
	Section string
	// Port, when non-zero, overrides Server.Port after everything else.
	Port uint
	// LookupEnv resolves ${NAME} placeholders. Defaults to os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// LoadFromEnv builds LoadOptions from environment variables and loads the config.
func LoadFromEnv() (*Config, error) {
	configPath := strings.TrimSpace(os.Getenv(EnvConfigPath))
	if configPath == "" {
		return nil, fmt.Errorf("%s is required", EnvConfigPath)
	}
	opts := LoadOptions{
		ConfigPath:       configPath,
		SecretConfigPath: strings.TrimSpace(os.Getenv(EnvSecretConfigPath)),
		OverrideTOML:     os.Getenv(EnvConfigOverride),
		Section:          strings.TrimSpace(os.Getenv(EnvConfigSection)),
	}
	if raw := strings.TrimSpace(os.Getenv(EnvPort)); raw != "" {
		port, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("invalid %s=%q: expected a port number", EnvPort, raw)
		}
		opts.Port = uint(port)
	}
	return Load(opts)
}

// Load reads, merges, expands, decodes and validates the configuration.
func Load(opts LoadOptions) (*Config, error) {
	if opts.ConfigPath == "" {
		return nil, errors.New("config path is required")
	}
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	sources := []struct{ name, text string }{}
	base, err := os.ReadFile(opts.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	sources = append(sources, struct{ name, text string }{opts.ConfigPath, string(base)})

	if opts.SecretConfigPath != "" {
		secret, err := os.ReadFile(opts.SecretConfigPath)
		if err != nil {
			return nil, fmt.Errorf("read secret config file: %w", err)
		}
		sources = append(sources, struct{ name, text string }{opts.SecretConfigPath, string(secret)})
	}
	if strings.TrimSpace(opts.OverrideTOML) != "" {
		sources = append(sources, struct{ name, text string }{"override", opts.OverrideTOML})
	}

	merged := map[string]any{}
	for _, src := range sources {
		tree, err := prepareSource(src.text, opts.Section, lookup)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src.name, err)
		}
		mergeTrees(merged, tree)
	}

	cfg, err := decodeAndValidate(merged)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if opts.Port != 0 {
		cfg.Server.Port = opts.Port
	}
	if err := validateSemantics(cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

var placeholderPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// prepareSource decodes one TOML source into a generic tree, optionally
// narrows it to a top-level table and expands ${NAME} placeholders in string
// values only (comments and keys are untouched).
func prepareSource(text, section string, lookup func(string) (string, bool)) (map[string]any, error) {
	var full map[string]any
	if _, err := toml.Decode(text, &full); err != nil {
		return nil, fmt.Errorf("decode TOML: %w", err)
	}
	if section != "" {
		sub, ok := full[section].(map[string]any)
		if !ok {
			return map[string]any{}, nil // this source has nothing for us
		}
		full = sub
	}

	missing := map[string]struct{}{}
	expandStrings(full, lookup, missing)
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unset environment variables referenced in config: %s", strings.Join(names, ", "))
	}
	return full, nil
}

// mergeTrees copies keys of override into base recursively. Nested tables are
// merged key by key; everything else (including arrays of tables) is replaced
// as a whole, so a secret file can swap a chain's target list but cannot patch
// a single target inside it.
func mergeTrees(base, override map[string]any) {
	for k, v := range override {
		if vMap, ok := v.(map[string]any); ok {
			if baseMap, ok := base[k].(map[string]any); ok {
				mergeTrees(baseMap, vMap)
				continue
			}
		}
		base[k] = v
	}
}

// decodeAndValidate turns the merged tree into Config, fills defaults from
// `default:"..."` tags and runs `validate:"..."` rules. Unknown keys are an
// error: a typo in a key must not silently fall back to a default.
func decodeAndValidate(tree map[string]any) (*Config, error) {
	var sb strings.Builder
	if err := toml.NewEncoder(&sb).Encode(tree); err != nil {
		return nil, fmt.Errorf("encode merged TOML: %w", err)
	}
	var cfg Config
	meta, err := toml.Decode(sb.String(), &cfg)
	if err != nil {
		return nil, fmt.Errorf("decode TOML: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	if err := defaults.Set(&cfg); err != nil {
		return nil, fmt.Errorf("apply defaults: %w", err)
	}
	if err := newValidator().Struct(cfg); err != nil {
		return nil, fmt.Errorf("validation: %w", describeValidationError(err))
	}
	return &cfg, nil
}

// expandStrings walks a decoded TOML tree and replaces ${NAME} inside string
// values. Names that are not set are collected into missing.
func expandStrings(node any, lookup func(string) (string, bool), missing map[string]struct{}) any {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			v[k] = expandStrings(child, lookup, missing)
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = expandStrings(child, lookup, missing)
		}
		return v
	case []map[string]any: // arrays of tables decode to this type
		for i, child := range v {
			expandStrings(child, lookup, missing)
			v[i] = child
		}
		return v
	case string:
		return placeholderPattern.ReplaceAllStringFunc(v, func(match string) string {
			name := placeholderPattern.FindStringSubmatch(match)[1]
			value, ok := lookup(name)
			if !ok {
				missing[name] = struct{}{}
				return match
			}
			return value
		})
	default:
		return node
	}
}
