// Command rpcgateway serves every configured chain from one HTTP port.
//
// Configuration comes from a TOML file (CONFIG_TOML_PATH) with optional
// overrides, see internal/config. Logging is controlled by LOG_LEVEL
// (debug|info|warn|error, default info) and LOG_FORMAT (json|console, default json).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/gateway"
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

// run keeps every defer (logger flush, signal cleanup) on the exit path.
func run() error {
	// A local .env is a convenience for developers; production uses real env vars.
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "warning: cannot load .env: %v\n", err)
	}

	log, err := newLogger()
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	defer func() { _ = log.Sync() }()
	log = log.With(zap.String("version", version), zap.String("commit", commit))

	cfg, err := config.LoadFromEnv()
	if err != nil {
		log.Error("configuration error", zap.Error(err))
		return err
	}
	log.Info("configuration loaded",
		zap.String("path", os.Getenv(config.EnvConfigPath)),
		zap.String("secret_path", os.Getenv(config.EnvSecretConfigPath)),
		zap.Strings("chains", cfg.ChainKeys()),
		zap.Uint("port", cfg.Server.Port))

	gw, err := gateway.New(cfg, log, events.NewLogger(log))
	if err != nil {
		log.Error("cannot build gateway", zap.Error(err))
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := gw.ListenAndServe(ctx); err != nil {
		log.Error("gateway stopped with error", zap.Error(err))
		return err
	}
	log.Info("gateway stopped")
	return nil
}

func newLogger() (*zap.Logger, error) {
	level := zapcore.InfoLevel
	if raw := strings.TrimSpace(os.Getenv("LOG_LEVEL")); raw != "" {
		if err := level.Set(strings.ToLower(raw)); err != nil {
			return nil, fmt.Errorf("LOG_LEVEL=%q: %w", raw, err)
		}
	}
	var zcfg zap.Config
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT"))) {
	case "", "json":
		zcfg = zap.NewProductionConfig()
		zcfg.Sampling = nil // every event matters for a gateway
	case "console":
		zcfg = zap.NewDevelopmentConfig()
	default:
		return nil, fmt.Errorf("LOG_FORMAT=%q: expected json or console", os.Getenv("LOG_FORMAT"))
	}
	zcfg.Level = zap.NewAtomicLevelAt(level)
	zcfg.OutputPaths = []string{"stdout"}
	return zcfg.Build()
}
