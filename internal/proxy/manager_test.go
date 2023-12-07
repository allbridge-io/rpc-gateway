package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

func runAccumulatedTests(f func() int) float64 {
	acc := 0.
	attempts := 128
	for i := 0; i < attempts; i++ {
		acc += float64(f()) / float64(attempts)
	}
	return acc
}

func TestHealthcheckManager(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	manager := NewHealthcheckManager(HealthcheckManagerConfig{
		Targets: []TargetConfig{
			{
				Name: "Primary",
				Connection: TargetConfigConnection{
					HTTP: TargetConnectionHTTP{
						URL: "https://cloudflare-eth.com",
					},
				},
			},
			{
				Name: "StandBy",
				Connection: TargetConfigConnection{
					HTTP: TargetConnectionHTTP{
						URL: "https://cloudflare-eth.com",
					},
				},
			},
		},

		Config: HealthCheckConfig{
			Interval:         1 * time.Second,
			Timeout:          1 * time.Second,
			FailureThreshold: 1,
			SuccessThreshold: 1,
		},
	})

	ctx := context.TODO()
	go manager.Start(ctx)

	acc := runAccumulatedTests(func() int {
		return manager.GetNextHealthyTargetIndex()
	})
	t.Logf("Average index when selecting from 2 options is %f", acc)
	// Random node selection must be aroung 50/50
	assert.InDelta(t, .5, acc, .25)

	time.Sleep(1 * time.Second)

	manager.TaintTarget("Primary")

	acc = runAccumulatedTests(func() int {
		return manager.GetNextHealthyTargetIndex()
	})
	t.Logf("Average index when selecting from 1 option is %f", acc)
	assert.Equal(t, 1., acc)

	manager.Stop(ctx)
}

func TestGetNextHealthyTargetIndexExcluding(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	manager := NewHealthcheckManager(HealthcheckManagerConfig{
		Targets: []TargetConfig{
			{
				Name: "Primary",
				Connection: TargetConfigConnection{
					HTTP: TargetConnectionHTTP{
						URL: "https://cloudflare-eth.com",
					},
				},
			},
			{
				Name: "Backup",
				Connection: TargetConfigConnection{
					HTTP: TargetConnectionHTTP{
						URL: "https://cloudflare-eth.com",
					},
				},
			},
		},

		Config: HealthCheckConfig{
			Interval:         200 * time.Millisecond,
			Timeout:          2000 * time.Millisecond,
			FailureThreshold: 1,
			SuccessThreshold: 1,
		},
	})

	ctx := context.TODO()

	go manager.Start(ctx)
	defer manager.Stop(ctx)

	accFromBoth := runAccumulatedTests(func() int {
		return manager.GetNextHealthyTargetIndexExcluding([]uint{})
	})
	accWithFirstExcluded := runAccumulatedTests(func() int {
		return manager.GetNextHealthyTargetIndexExcluding([]uint{0})
	})
	accWithSecondExcluded := runAccumulatedTests(func() int {
		return manager.GetNextHealthyTargetIndexExcluding([]uint{1})
	})
	t.Logf("Both: %f, no first: %f, no second: %f", accFromBoth, accWithFirstExcluded, accWithSecondExcluded)
	assert.InDelta(t, .5, accFromBoth, .25)
	assert.Equal(t, 1., accWithFirstExcluded)
	assert.Equal(t, 0., accWithSecondExcluded)

	manager.GetTargetByName("Primary").Taint()

	assert.Equal(t, 1., runAccumulatedTests(func() int {
		return manager.GetNextHealthyTargetIndexExcluding([]uint{})
	}))
}
