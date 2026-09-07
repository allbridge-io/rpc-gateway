// Package gateway wires the configuration into per-chain proxies and serves
// them all from one HTTP listener.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/0xProject/rpc-gateway/internal/config"
	"github.com/0xProject/rpc-gateway/internal/events"
	"github.com/0xProject/rpc-gateway/internal/proxy"
)

// Chain is one served network.
type Chain struct {
	Key     string
	Type    config.ChainType
	Proxy   *proxy.Proxy
	Manager *proxy.Manager
}

// Gateway owns every chain and the HTTP handler in front of them.
type Gateway struct {
	cfg      *config.Config
	log      *zap.Logger
	chains   map[string]*Chain // upper-cased key
	order    []string          // config keys, sorted
	handler  http.Handler
	listenOK chan struct{}
	addrMu   sync.Mutex
	addr     string
}

// New builds a Gateway from cfg. Nothing is started yet.
func New(cfg *config.Config, log *zap.Logger, observer events.Observer) (*Gateway, error) {
	if log == nil {
		log = zap.NewNop()
	}
	if observer == nil {
		observer = events.Nop{}
	}
	g := &Gateway{cfg: cfg, log: log, chains: map[string]*Chain{}, order: cfg.ChainKeys(), listenOK: make(chan struct{})}

	for _, key := range g.order {
		chainCfg := cfg.Chains[key]
		manager := proxy.NewManager(key, chainCfg.Type, chainCfg.Targets, proxy.HealthOptions{
			Interval:         cfg.HealthChecks.Interval,
			Timeout:          cfg.HealthChecks.Timeout,
			FailureThreshold: cfg.HealthChecks.FailureThreshold,
			SuccessThreshold: cfg.HealthChecks.SuccessThreshold,
			MaxBlockLag:      cfg.MaxBlockLagFor(key),
			TaintDuration:    cfg.HealthChecks.TaintDurationOrDefault(),
		}, observer)
		p, err := proxy.New(proxy.Options{
			Chain:           key,
			Type:            chainCfg.Type,
			Targets:         chainCfg.Targets,
			Exceptions:      cfg.ExceptionsFor(key),
			UpstreamTimeout: cfg.Server.UpstreamTimeout,
			Observer:        observer,
		}, manager)
		if err != nil {
			return nil, fmt.Errorf("chain %s: %w", key, err)
		}
		g.chains[strings.ToUpper(key)] = &Chain{Key: key, Type: chainCfg.Type, Proxy: p, Manager: manager}
	}
	g.handler = g.newRouter()
	return g, nil
}

// Chain looks a chain up by its URL segment, case-insensitively.
func (g *Gateway) Chain(key string) *Chain {
	return g.chains[strings.ToUpper(key)]
}

// Handler returns the HTTP handler serving every route.
func (g *Gateway) Handler() http.Handler { return g.handler }

// RunHealthChecksOnce performs one check round for every chain, concurrently.
func (g *Gateway) RunHealthChecksOnce(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range g.chains {
		wg.Add(1)
		go func(c *Chain) {
			defer wg.Done()
			c.Manager.RunOnce(ctx)
		}(c)
	}
	wg.Wait()
}

// StartHealthChecks starts the periodic checks of every chain until ctx ends.
// It does not run a round immediately: ListenAndServe already did one, and
// checking every target twice at startup would only waste provider quota.
func (g *Gateway) StartHealthChecks(ctx context.Context) {
	for _, c := range g.chains {
		go c.Manager.StartTicker(ctx)
	}
}

// ListenAndServe runs the gateway until ctx is cancelled: one initial health
// round, periodic checks, the HTTP server, and a graceful shutdown bounded by
// ShutdownTimeout.
//
// The initial round fills /status and the block numbers before the first
// request, but it is one round: a dead target is only excluded once
// FailureThreshold consecutive checks failed (2 by default), so it may still
// receive the first requests. Those fail over and taint it, so clients are
// not affected; set failure_threshold = 1 to exclude it from the start.
func (g *Gateway) ListenAndServe(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g.log.Info("initial health check round", zap.Strings("chains", g.order))
	g.RunHealthChecksOnce(ctx)
	g.logHealthSummary()
	g.StartHealthChecks(ctx)

	addr := fmt.Sprintf(":%d", g.cfg.Server.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	g.addrMu.Lock()
	g.addr = listener.Addr().String()
	g.addrMu.Unlock()
	close(g.listenOK)

	server := &http.Server{
		Handler:           g.handler,
		ReadTimeout:       g.cfg.Server.ReadTimeout,
		WriteTimeout:      g.cfg.Server.WriteTimeout,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		g.log.Info("listening", zap.String("addr", listener.Addr().String()), zap.Strings("chains", g.order))
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	g.log.Info("shutting down", zap.Duration("timeout", g.cfg.Server.ShutdownTimeout))
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), g.cfg.Server.ShutdownTimeout)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// Addr returns the bound address once ListenAndServe is listening (tests use port 0).
func (g *Gateway) Addr(ctx context.Context) (string, error) {
	select {
	case <-g.listenOK:
		g.addrMu.Lock()
		defer g.addrMu.Unlock()
		return g.addr, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (g *Gateway) logHealthSummary() {
	for _, key := range g.order {
		c := g.Chain(key)
		routable := 0
		for _, s := range c.Manager.Status() {
			if s.Routable {
				routable++
			}
		}
		g.log.Info("chain ready", zap.String("chain", key), zap.Int("routable_targets", routable), zap.Int("targets", c.Manager.Len()))
	}
}
