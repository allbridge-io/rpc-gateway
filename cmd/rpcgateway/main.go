package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/0xProject/rpc-gateway/internal/metrics"
	"github.com/0xProject/rpc-gateway/internal/rpcgateway"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/sync/errgroup"
)

func setupLogger() {
	stdout := zapcore.AddSync(os.Stdout)
	debugLogEnabled := os.Getenv("DEBUG") == "true"

	level := zap.NewAtomicLevelAt(zap.InfoLevel)
	if debugLogEnabled {
		level = zap.NewAtomicLevelAt(zap.DebugLevel)
	}

	developmentConfig := zap.NewDevelopmentEncoderConfig()
	developmentConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	developmentConfig.EncodeTime = zapcore.RFC3339TimeEncoder

	consoleEncoder := zapcore.NewConsoleEncoder(developmentConfig)

	core := zapcore.NewTee(
		zapcore.NewCore(consoleEncoder, stdout, level),
	)

	logger := zap.New(core)
	zap.ReplaceGlobals(logger)
}

func main() {
	topCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	setupLogger()

	defer func() {
		err := zap.L().Sync() // flushes buffer, if any
		if err != nil {
			zap.L().Error("failed to flush logger with err: %s", zap.Error(err))
		}
	}()

	g, gCtx := errgroup.WithContext(topCtx)

	// Initialize config
	configFileLocation := flag.String("config", "./config.yml", "path to rpc gateway config file")
	flag.Parse()
	config, err := rpcgateway.NewRPCGatewayFromConfigFile(*configFileLocation)
	if err != nil {
		zap.L().Fatal("failed to get config", zap.Error(err))
	}

	// start gateway
	rpcGateway := rpcgateway.NewRPCGateway(*config)

	// start healthz and metrics server
	metricsServer := metrics.NewServer(config.Metrics)
	g.Go(func() error {
		return metricsServer.Start()
	})

	g.Go(func() error {
		return rpcGateway.Start(context.TODO())
	})

	g.Go(func() error {
		<-gCtx.Done()
		err := metricsServer.Stop()
		if err != nil {
			zap.L().Error("error when stopping healthserver", zap.Error(err))
		}
		err = rpcGateway.Stop(context.TODO())
		if err != nil {
			zap.L().Error("error when stopping rpc gateway", zap.Error(err))
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		fmt.Printf("exit reason: %s \n", err)
	}
}
