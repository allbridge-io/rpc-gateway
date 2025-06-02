package proxy

import (
	"time"
)

type HealthCheckConfig struct {
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	FailureThreshold uint          `yaml:"failureThreshold"`
	SuccessThreshold uint          `yaml:"successThreshold"`
}

type ProxyConfig struct {
	Port            string        `yaml:"port"`
	UpstreamTimeout time.Duration `yaml:"upstreamTimeout"`
	EnableRandomization bool      `yaml:"enableRandomization"`
}

type TargetConnectionHTTP struct {
	URL               string `yaml:"url"`
	Compression       bool   `yaml:"compression"`
	DisableKeepAlives bool   `yaml:"disableKeepAlives"`
}

type TargetConnectionWS struct {
	URL string `yaml:"url"`
}

type TargetConfigConnection struct {
	HTTP TargetConnectionHTTP `yaml:"http"`
	WS   TargetConnectionWS   `yaml:"ws"`
}

type Exception struct {
	Match   string `yaml:"match"`
	Message string `yaml:"message"`
}

type TargetConfig struct {
	Name       string                 `yaml:"name"`
	Connection TargetConfigConnection `yaml:"connection"`
}

// This struct is temporary. It's about to keep the input interface clean and simple.
type Config struct {
	Proxy        ProxyConfig
	Targets      []TargetConfig
	HealthChecks HealthCheckConfig
	Exceptions   []Exception
	Solana       bool
}
