// Package telemetry ships logs and metrics to an OTLP endpoint such as the
// Grafana Cloud OTLP gateway, directly from the process (no agent).
//
// It is enabled only when OTEL_EXPORTER_OTLP_ENDPOINT is set. All connection
// settings are the standard OpenTelemetry environment variables read by the
// SDK: OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_EXPORTER_OTLP_HEADERS
// (Authorization=Basic ... for Grafana Cloud), OTEL_SERVICE_NAME,
// OTEL_RESOURCE_ATTRIBUTES, OTEL_METRIC_EXPORT_INTERVAL. Only OTLP over HTTP
// is supported.
//
// Logs: a zap core bridged to the OTel log SDK, so every structured log line
// at OTLP_LOG_LEVEL or above (default info) is exported, including the events
// emitted by events.Logger. Metrics: an events.Observer that counts upstream
// requests, reroutes, taints and exposes per-target health gauges.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Environment variables specific to this package (the rest are OTel standard).
const (
	EnvEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvLogLevel = "OTLP_LOG_LEVEL" // minimum zap level exported as OTLP logs, default info
)

// Options describe the service to the backend.
type Options struct {
	ServiceName string // default rpc-gateway (OTEL_SERVICE_NAME wins)
	Version     string
	// ErrorLog receives exporter errors (connection refused, 401, 429...). It
	// must not itself be bridged to OTLP, or a failing export would loop.
	ErrorLog *zap.Logger
	// Now is used for metric timestamps in tests. Optional.
	Now func() time.Time
}

// Telemetry owns the OTel providers.
type Telemetry struct {
	logs    *sdklog.LoggerProvider
	metrics *sdkmetric.MeterProvider
}

// Enabled reports whether an OTLP endpoint is configured.
func Enabled() bool {
	return strings.TrimSpace(os.Getenv(EnvEndpoint)) != ""
}

// Setup builds the log and metric providers with OTLP/HTTP exporters. Nothing
// is sent until the batch processors flush (every few seconds for logs,
// OTEL_METRIC_EXPORT_INTERVAL for metrics, default 60s).
func Setup(ctx context.Context, opts Options) (*Telemetry, error) {
	if opts.ServiceName == "" {
		opts.ServiceName = "rpc-gateway"
	}
	if opts.ErrorLog == nil {
		opts.ErrorLog = zap.NewNop()
	}
	errLog := opts.ErrorLog.Named("otel")
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		errLog.Warn("export error", zap.Error(err))
	}))

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(opts.ServiceName),
			semconv.ServiceVersion(opts.Version),
		),
		resource.WithFromEnv(), // OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES override
		resource.WithTelemetrySDK(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	logExporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp log exporter: %w", err)
	}
	logs := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
	)

	metricExporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		_ = logs.Shutdown(ctx)
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	metrics := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
	)

	return &Telemetry{logs: logs, metrics: metrics}, nil
}

// ZapCore returns a zap core that exports records at or above level over OTLP.
// Add it to the application logger with zapcore.NewTee.
func (t *Telemetry) ZapCore(level zapcore.Level) (zapcore.Core, error) {
	core := otelzap.NewCore("rpc-gateway", otelzap.WithLoggerProvider(t.logs))
	filtered, err := zapcore.NewIncreaseLevelCore(core, level)
	if err != nil {
		return nil, fmt.Errorf("otlp log level: %w", err)
	}
	return filtered, nil
}

// LogLevelFromEnv reads OTLP_LOG_LEVEL (default info).
func LogLevelFromEnv() (zapcore.Level, error) {
	level := zapcore.InfoLevel
	if raw := strings.TrimSpace(os.Getenv(EnvLogLevel)); raw != "" {
		if err := level.Set(strings.ToLower(raw)); err != nil {
			return level, fmt.Errorf("%s=%q: %w", EnvLogLevel, raw, err)
		}
	}
	return level, nil
}

// ForceFlush pushes buffered logs and metrics now (tests, shutdown).
func (t *Telemetry) ForceFlush(ctx context.Context) error {
	return errors.Join(t.logs.ForceFlush(ctx), t.metrics.ForceFlush(ctx))
}

// Shutdown flushes and stops both providers.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	return errors.Join(t.logs.Shutdown(ctx), t.metrics.Shutdown(ctx))
}
