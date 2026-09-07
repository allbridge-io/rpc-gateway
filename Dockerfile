FROM golang:1.26-alpine AS builder

RUN apk add --update-cache --no-cache git build-base

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags "-s -w" -o /rpc-gateway ./cmd/rpcgateway

FROM alpine:3.21

RUN apk add --update-cache --no-cache ca-certificates

COPY --from=builder /rpc-gateway /app/rpc-gateway

USER nobody
ENV CONFIG_TOML_PATH=/config/config.toml
EXPOSE 3000
CMD ["/app/rpc-gateway"]
