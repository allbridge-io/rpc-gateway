.DEFAULT_GOAL := build

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS ?= -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build build-render run test test-race test-testnet test-testnet-compile vet tidy

## Build the binary into ./rpc-gateway
build:
	go build -ldflags "$(LDFLAGS)" -o rpc-gateway ./cmd/rpcgateway

## Build for Render (static binary named ./app, see render.yaml)
build-render:
	go build -tags netgo -ldflags "$(LDFLAGS)" -o app ./cmd/rpcgateway

## Run locally; reads .env (CONFIG_TOML_PATH etc.)
run:
	go run ./cmd/rpcgateway

## Fast, offline unit tests
test:
	go test -count=1 ./...

## Same with the race detector (what CI runs)
test-race:
	go test -race -shuffle=on -count=1 ./...

## Real-network checks against public testnets (tests/testnet/config.testnet.toml).
## TESTNET_CONFIG_TOML_PATH / SECRET_CONFIG_TOML_PATH point at your own config; TESTNET_CHAINS=SOL,TRX filters.
test-testnet:
	go test -tags testnet -run '^TestTestnet' -count=1 -v -timeout 10m ./tests/testnet/...

## Compile the testnet tests without running them (CI)
test-testnet-compile:
	go test -tags testnet -run '^$$' ./tests/testnet/...

vet:
	go vet ./...
	go vet -tags testnet ./tests/testnet/...

tidy:
	go mod tidy
