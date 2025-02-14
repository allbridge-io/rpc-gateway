package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mwitkow/go-conntrack"
	"github.com/pkg/errors"

	"go.uber.org/zap"
)

// TODO
// This code needs a new abstraction. We should bring a model and attach helper to a model.
//

func doProcessRequest(r *http.Request, config TargetConfig) error {
	var body io.Reader
	var buf bytes.Buffer
	var err error

	if r.Header.Get("Upgrade") != "" {
		return nil
	}

	if r.Body == nil {
		return errors.New("no body")
	}

	// The standard library stores ContentLength as signed data type.
	//
	if r.ContentLength == 0 || r.ContentLength < 0 {
		return errors.New("invalid content length")
	}

	if r.Header.Get("Content-Encoding") == "gzip" && !config.Connection.HTTP.Compression {
		body, err = doGunzip(r)
		if err != nil {
			return errors.Wrap(err, "cannot gunzip data")
		}
	} else {
		body = io.TeeReader(r.Body, &buf)
	}

	// I don't like so much but the refactor is coming up soon!
	//
	// This is nothing more than ugly a workaround.
	// This code guarantee the context buf will not be empty upon primary
	// provider roundtrip failures.
	//
	data, err := io.ReadAll(body)
	if err != nil {
		return errors.New("cannot read body")
	}

	r.Body = io.NopCloser(bytes.NewBuffer(data))

	// Here's an interesting fact. There's no data in buf, until a call
	// to Read(). With Read() call, it will write data to bytes.Buffer.
	//
	// I want to call it out, because it's damn smart.
	//
	ctx := context.WithValue(r.Context(), "bodybuf", &buf) // nolint:revive,staticcheck

	// WithContext creates a shallow copy. It's highly important to
	// override underlying memory pointed by pointer.
	//
	r2 := r.WithContext(ctx)
	*r = *r2

	return nil
}

func doGunzip(r *http.Request) (io.Reader, error) {
	var buf bytes.Buffer
	var body io.Reader

	uncompressed, err := gzip.NewReader(r.Body)
	if err != nil {
		return nil, errors.Wrap(err, "cannot decompress the data")
	}
	// Decompress the body.
	//
	data, err := io.ReadAll(uncompressed)
	if err != nil {
		return nil, errors.Wrap(err, "cannot read uncompressed data")
	}

	// Replace body content with uncompressed data
	// Remove the "Content-Encoding: gzip" because the body is decompressed already
	// and correct the Content-Length header
	//
	body = io.TeeReader(bytes.NewReader(data), &buf)

	r.Header.Del("Content-Encoding")
	r.ContentLength = int64(len(data))

	return body, nil
}

func cloneBody(r *http.Response) (io.Reader, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, errors.Wrap(err, "cannot read body")
	}

	r.Body = io.NopCloser(bytes.NewBuffer(data))

	return io.NopCloser(bytes.NewBuffer(data)), nil
}

func getResponseBody(resp *http.Response, config TargetConfig) (string, error) {
	var body io.Reader
	clonedBody, err := cloneBody(resp)
	if err != nil {
		return "", err
	}

	if resp.Header.Get("Content-Encoding") == "gzip" && !config.Connection.HTTP.Compression {
		body, err = gzip.NewReader(clonedBody)
		if err != nil {
			return "", errors.Wrap(err, "cannot read a body")
		}

	} else {
		body = clonedBody
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return "", errors.Wrap(err, "cannot read body")
	}

	return string(data), nil
}

func NewReverseProxy(targetConfig TargetConfig, config Config) (*httputil.ReverseProxy, *httputil.ReverseProxy, error) {
	target, err := url.Parse(targetConfig.Connection.HTTP.URL)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot parse url")
	}

	var wsProxy *httputil.ReverseProxy
	if config.Solana {
		wsUrl := targetConfig.Connection.WS.URL
		if wsUrl == "" {
			wsUrl = targetConfig.Connection.HTTP.URL
		}

		wsUrl = regexp.MustCompile("^ws").ReplaceAllLiteralString(wsUrl, `http`)
		wsTarget, err := url.Parse(wsUrl)
		if err != nil {
			wsTarget = target
			splittedHost := strings.Split(wsTarget.Host, ":")
			if len(splittedHost) == 2 {
				portNum, err := strconv.Atoi(splittedHost[1])
				if err != nil {
					zap.L().Error("Failed parse port number", zap.Error(err))
				}
				wsTarget.Host = fmt.Sprintf("%s:%d", splittedHost[0], portNum+1)
			}
			return nil, nil, errors.Wrap(err, "cannot parse url")
		}

		wsProxy = httputil.NewSingleHostReverseProxy(target)
		wsProxy.Director = func(r *http.Request) {
			r.Host = wsTarget.Host
			r.URL.Scheme = wsTarget.Scheme
			r.URL.Host = wsTarget.Host
			r.URL.Path = wsTarget.Path
			r.URL.RawQuery = target.RawQuery

			// Workaround to reserve request body in ReverseProxy.ErrorHandler
			// see more here: https://github.com/golang/go/issues/33726
			//
			if err := doProcessRequest(r, targetConfig); err != nil {
				zap.L().Error("cannot process request", zap.Error(err))
			}

			zap.L().Debug("request forward", zap.String("WS", r.URL.String()))
		}
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(r *http.Request) {
		r.Host = target.Host
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		r.URL.Path = target.Path
		r.URL.RawQuery = target.RawQuery

		// Workaround to reserve request body in ReverseProxy.ErrorHandler
		// see more here: https://github.com/golang/go/issues/33726
		//
		if err := doProcessRequest(r, targetConfig); err != nil {
			zap.L().Error("cannot process request", zap.Error(err))
		}

		zap.L().Debug("request forward", zap.String("URL", r.URL.String()))
	}

	conntrackDialer := conntrack.NewDialContextFunc(
		conntrack.DialWithName(targetConfig.Name),
		conntrack.DialWithTracing(),
		conntrack.DialWithDialer(&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}),
	)

	proxy.Transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           conntrackDialer,
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     targetConfig.Connection.HTTP.DisableKeepAlives,
		MaxIdleConns:          100,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: config.Proxy.UpstreamTimeout,
	}

	conntrack.PreRegisterDialerMetrics(targetConfig.Name)

	return proxy, wsProxy, nil
}
