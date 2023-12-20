package proxy

import (
	"context"
	"math/rand"
	"strconv"
	"time"

	"slices"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

type HealthcheckManagerConfig struct {
	Targets []TargetConfig
	Config  HealthCheckConfig
	Solana  bool
}

type HealthcheckManager struct {
	healthcheckers []Healthchecker

	metricRPCProviderInfo        *prometheus.GaugeVec
	metricRPCProviderStatus      *prometheus.GaugeVec
	metricResponseTime           *prometheus.HistogramVec
	metricRPCProviderBlockNumber *prometheus.GaugeVec
	metricRPCProviderGasLimit    *prometheus.GaugeVec
	metricRPCResponseStatus      *prometheus.CounterVec
}

func NewHealthcheckManager(config HealthcheckManagerConfig) *HealthcheckManager {
	healthCheckers := []Healthchecker{}

	healthcheckManager := &HealthcheckManager{
		metricRPCProviderInfo: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "zeroex_rpc_gateway_provider_info",
				Help: "Gas limit of a given provider",
			}, []string{
				"index",
				"provider",
			}),
		metricRPCProviderStatus: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "zeroex_rpc_gateway_provider_status",
				Help: "Current status of a given provider by type. Type can be either healthy or tainted.",
			}, []string{
				"provider",
				"type",
			}),
		metricResponseTime: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "zeroex_rpc_gateway_healthcheck_response_duration_seconds",
				Help: "Histogram of response time for Gateway Healthchecker in seconds",
				Buckets: []float64{
					.005,
					.01,
					.025,
					.05,
					.1,
					.25,
					.5,
					1,
					2.5,
					5,
					10,
				},
			}, []string{
				"provider",
				"method",
			}),
		metricRPCProviderBlockNumber: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "zeroex_rpc_gateway_provider_block_number",
				Help: "Block number of a given provider",
			}, []string{
				"provider",
			}),
		metricRPCProviderGasLimit: promauto.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "zeroex_rpc_gateway_provider_gasLimit_number",
				Help: "Gas limit of a given provider",
			}, []string{
				"provider",
			}),
		metricRPCResponseStatus: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "zeroex_rpc_gateway_provider_response_status",
				Help: "Gas limit of a given provider",
			}, []string{
				"provider",
				"status",
			}),
	}

	for _, target := range config.Targets {
		healthchecker, err := NewHealthchecker(
			RPCHealthcheckerConfig{
				URL:              target.Connection.HTTP.URL,
				Name:             target.Name,
				Solana:           config.Solana,
				Interval:         config.Config.Interval,
				Timeout:          config.Config.Timeout,
				FailureThreshold: config.Config.FailureThreshold,
				SuccessThreshold: config.Config.SuccessThreshold,
			})

		healthchecker.SetMetric(MetricBlockNumber, healthcheckManager.metricRPCProviderBlockNumber)
		healthchecker.SetMetric(MetricGasLimit, healthcheckManager.metricRPCProviderGasLimit)
		healthchecker.SetMetric(MetricResponseTime, healthcheckManager.metricResponseTime)
		healthchecker.SetMetric(MetricRPCResponseStatus, healthcheckManager.metricRPCResponseStatus)

		if err != nil {
			panic(err)
		}

		healthCheckers = append(healthCheckers, healthchecker)
	}

	healthcheckManager.healthcheckers = healthCheckers

	return healthcheckManager
}

func (h *HealthcheckManager) runLoop(ctx context.Context) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			h.reportStatusMetrics()
		}
	}
}

func (h *HealthcheckManager) reportStatusMetrics() {
	for _, healthchecker := range h.healthcheckers {
		h.metricRPCProviderStatus.WithLabelValues(healthchecker.Name(), "healthy").Set(float64(boolToInt(healthchecker.IsHealthy())))
		h.metricRPCProviderStatus.WithLabelValues(healthchecker.Name(), "tainted").Set(float64(boolToInt(healthchecker.IsTainted())))
	}
}

func (h *HealthcheckManager) Start(ctx context.Context) error {
	for index, healthChecker := range h.healthcheckers {
		h.metricRPCProviderInfo.WithLabelValues(strconv.Itoa(index), healthChecker.Name()).Set(1)
		go healthChecker.Start(ctx)
	}

	return h.runLoop(ctx)
}

func (h *HealthcheckManager) Stop(ctx context.Context) error {
	for _, healthChecker := range h.healthcheckers {
		err := healthChecker.Stop(ctx)
		if err != nil {
			zap.L().Error("healtchecker stop error", zap.Error(err))
		}
	}

	return nil
}

func (h *HealthcheckManager) GetTargetIndexByName(name string) int {
	for idx, healthChecker := range h.healthcheckers {
		if healthChecker.Name() == name {
			return idx
		}
	}

	zap.L().Error("tried to access a non-existing Healthchecker", zap.String("name", name))
	return 0
}

func (h *HealthcheckManager) GetTargetByName(name string) Healthchecker {
	for _, healthChecker := range h.healthcheckers {
		if healthChecker.Name() == name {
			return healthChecker
		}
	}

	zap.L().Error("tried to access a non-existing Healthchecker", zap.String("name", name))
	return nil
}

func (h *HealthcheckManager) TaintTarget(name string) {
	if healthChecker := h.GetTargetByName(name); healthChecker != nil {
		healthChecker.Taint()
		return
	}
}

func (h *HealthcheckManager) IsTargetHealthy(name string) bool {
	if healthChecker := h.GetTargetByName(name); healthChecker != nil {
		return healthChecker.IsHealthy()
	}

	return false
}

func (h *HealthcheckManager) GetNextHealthyTargetIndex() int {
	return h.GetNextHealthyTargetIndexExcluding([]uint{})
}

func (h *HealthcheckManager) GetNextHealthyTargetIndexExcluding(excludedIdx []uint) int {

	totalTargets := len(h.healthcheckers)
	if totalTargets == 0 {
		zap.L().Error("no targets")
		return -1
	}

	idx := rand.Intn(totalTargets)
	delta := 0
	for delta < totalTargets {
		adjustedIndex := (idx + delta) % totalTargets
		target := h.healthcheckers[adjustedIndex]
		if !slices.Contains(excludedIdx, uint(adjustedIndex)) && target.IsHealthy() {
			return adjustedIndex
		}
		delta++
	}

	// no healthy targets, we down:(
	zap.L().Error("no more healthy targets")
	return -1
}
