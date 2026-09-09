// Command rpcgateway serves every configured chain from one HTTP port.
//
// Configuration comes from a TOML file (CONFIG_TOML_PATH) with optional
// overrides, see internal/config. Logging is controlled by LOG_LEVEL
// (debug|info|warn|error, default info) and LOG_FORMAT (json|console, default
// json). When OTEL_EXPORTER_OTLP_ENDPOINT is set, logs and metrics are also
// pushed over OTLP (Grafana Cloud), see internal/telemetry.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/gateway"
	"github.com/0xProject/rpc-gateway/internal/telemetry"
)

// Set by the Makefile through -ldflags.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rpc-gateway: %v\n", err)
		os.Exit(1)
	}
}

// run keeps every defer (logger flush, telemetry shutdown) on the exit path.
func run() error {
	// A local .env is a convenience for developers; production uses real env vars.
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "warning: cannot load .env: %v\n", err)
	}

	stdoutCore, err := newStdoutCore()
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	// stdoutLog never goes to OTLP: telemetry reports its own export errors here.
	stdoutLog := zap.New(stdoutCore, zap.AddCaller()).With(zap.String("version", version), zap.String("commit", commit))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cores := []zapcore.Core{stdoutCore}
	var tel *telemetry.Telemetry
	if telemetry.Enabled() {
		otlpLevel, err := telemetry.LogLevelFromEnv()
		if err != nil {
			return err
		}
		tel, err = telemetry.Setup(ctx, telemetry.Options{ServiceName: "rpc-gateway", Version: version, ErrorLog: stdoutLog})
		if err != nil {
			return fmt.Errorf("telemetry: %w", err)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := tel.Shutdown(shutdownCtx); err != nil {
				stdoutLog.Warn("telemetry shutdown", zap.Error(err))
			}
		}()
		otlpCore, err := tel.ZapCore(otlpLevel)
		if err != nil {
			return err
		}
		cores = append(cores, otlpCore)
		stdoutLog.Info("OTLP telemetry enabled",
			zap.String("endpoint", os.Getenv(telemetry.EnvEndpoint)), zap.Stringer("otlp_log_level", otlpLevel))
	}
	log := zap.New(zapcore.NewTee(cores...), zap.AddCaller()).With(zap.String("version", version), zap.String("commit", commit))
	defer func() { _ = log.Sync() }()

	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Error("configuration error", zap.Error(err))
		return err
	}
	log.Info("configuration loaded",
		zap.String("path", os.Getenv(config.EnvConfigPath)),
		zap.String("secret_path", os.Getenv(config.EnvSecretConfigPath)),
		zap.Strings("chains", cfg.ChainKeys()),
		zap.Uint("port", cfg.Server.Port),
		zap.Int("api_keys", len(cfg.Server.APIKeys)))
	if !cfg.Server.AuthEnabled() {
		log.Warn("no server.api_keys configured: every route is open to anyone who can reach the port")
	}

	var observer events.Observer = events.NewLogger(log)
	var metrics *telemetry.Metrics
	if tel != nil {
		// The gauges need the gateway, which needs the observer: the snapshot
		// source is installed once the gateway exists (SetSnapshot below).
		metrics, err = tel.Metrics(nil)
		if err != nil {
			return fmt.Errorf("telemetry metrics: %w", err)
		}
		observer = events.Multi{observer, metrics}
	}

	gw, err := gateway.New(cfg, log, observer)
	if err != nil {
		log.Error("cannot build gateway", zap.Error(err))
		return err
	}
	if metrics != nil {
		metrics.SetSnapshot(func() []telemetry.TargetSnapshot { return snapshots(gw.Status()) })
	}

	if err := gw.ListenAndServe(ctx); err != nil {
		log.Error("gateway stopped with error", zap.Error(err))
		return err
	}
	log.Info("gateway stopped")
	return nil
}

func snapshots(st gateway.Status) []telemetry.TargetSnapshot {
	var out []telemetry.TargetSnapshot
	for chain, c := range st.Chains {
		for _, t := range c.Targets {
			out = append(out, telemetry.TargetSnapshot{
				Chain: chain, Target: t.Name, Routable: t.Routable, BlockNumber: t.BlockNumber, Lag: t.Lag,
			})
		}
	}
	return out
}

// newStdoutCore builds the console/JSON core from LOG_LEVEL and LOG_FORMAT.
func newStdoutCore() (zapcore.Core, error) {
	level := zapcore.InfoLevel
	if raw := strings.TrimSpace(os.Getenv("LOG_LEVEL")); raw != "" {
		if err := level.Set(strings.ToLower(raw)); err != nil {
			return nil, fmt.Errorf("LOG_LEVEL=%q: %w", raw, err)
		}
	}
	var enc zapcore.Encoder
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT"))) {
	case "", "json":
		encCfg := zap.NewProductionEncoderConfig()
		encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
		enc = zapcore.NewJSONEncoder(encCfg)
	case "console":
		enc = zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	default:
		return nil, fmt.Errorf("LOG_FORMAT=%q: expected json or console", os.Getenv("LOG_FORMAT"))
	}
	return zapcore.NewCore(enc, zapcore.Lock(os.Stdout), level), nil
}
