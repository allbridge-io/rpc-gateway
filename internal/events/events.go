// Package events defines the notifications the gateway core emits about
// upstream targets and requests, and the default implementation that writes
// them to the structured log.
//
// The core never logs or measures anything itself: it calls an Observer.
// This keeps the proxy testable (tests plug in a recording observer) and lets
// us add an OTLP exporter for Grafana Cloud next to the logger without
// touching routing code.
package events

import (
	"time"

	"go.uber.org/zap"
)

// Observer receives notable events from the gateway core. Implementations must
// be safe for concurrent use and must not block.
type Observer interface {
	// TargetHealthChanged fires when a target becomes routable or stops being
	// routable because of health checks (failures, recovery, block lag).
	TargetHealthChanged(chain, target string, healthy bool, reason string)
	// TargetTainted fires when a target is temporarily excluded after a failed request.
	TargetTainted(chain, target, reason string, duration time.Duration)
	// RequestRerouted fires when a request to target failed and will be retried elsewhere.
	RequestRerouted(chain, target, reason string)
	// NoHealthyTargets fires when a request could not be served at all.
	NoHealthyTargets(chain string, attempted int)
	// UpstreamRequest fires after every attempt to a target, successful or not.
	UpstreamRequest(chain, target, method string, status int, duration time.Duration, err error)
}

// Nop discards every event.
type Nop struct{}

func (Nop) TargetHealthChanged(string, string, bool, string)                  {}
func (Nop) TargetTainted(string, string, string, time.Duration)               {}
func (Nop) RequestRerouted(string, string, string)                            {}
func (Nop) NoHealthyTargets(string, int)                                      {}
func (Nop) UpstreamRequest(string, string, string, int, time.Duration, error) {}

// Logger writes events to a zap logger as structured records.
type Logger struct {
	log *zap.Logger
}

// NewLogger returns an Observer backed by log.
func NewLogger(log *zap.Logger) *Logger {
	return &Logger{log: log.Named("events")}
}

func (l *Logger) TargetHealthChanged(chain, target string, healthy bool, reason string) {
	fields := []zap.Field{zap.String("chain", chain), zap.String("target", target), zap.String("reason", reason)}
	if healthy {
		l.log.Info("target healthy", fields...)
	} else {
		l.log.Warn("target unhealthy", fields...)
	}
}

func (l *Logger) TargetTainted(chain, target, reason string, duration time.Duration) {
	l.log.Warn("target tainted",
		zap.String("chain", chain), zap.String("target", target),
		zap.String("reason", reason), zap.Duration("duration", duration))
}

func (l *Logger) RequestRerouted(chain, target, reason string) {
	l.log.Warn("request rerouted",
		zap.String("chain", chain), zap.String("target", target), zap.String("reason", reason))
}

func (l *Logger) NoHealthyTargets(chain string, attempted int) {
	l.log.Error("no healthy targets", zap.String("chain", chain), zap.Int("attempted", attempted))
}

func (l *Logger) UpstreamRequest(chain, target, method string, status int, duration time.Duration, err error) {
	fields := []zap.Field{
		zap.String("chain", chain), zap.String("target", target), zap.String("method", method),
		zap.Int("status", status), zap.Duration("duration", duration),
	}
	if err != nil {
		l.log.Debug("upstream request failed", append(fields, zap.Error(err))...)
		return
	}
	l.log.Debug("upstream request", fields...)
}
