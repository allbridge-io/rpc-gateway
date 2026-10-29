package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// HTTP/2 multiplexes every request to a host over one connection. When that
// connection dies silently (a middlebox forgets it, the peer stops reading),
// TCP does not notice for minutes and every request sent over it hangs until
// its own timeout: a provider looks down while a fresh connection would work.
// With pings on, a connection that has received no frame for h2SendPingTimeout
// is pinged and closed if the ping is not answered within h2PingTimeout, so
// the requests after it dial a new one.
const (
	h2SendPingTimeout = 5 * time.Second
	h2PingTimeout     = 5 * time.Second
)

// http2WithPings is the HTTP/2 configuration of every upstream transport.
func http2WithPings() *http.HTTP2Config {
	return &http.HTTP2Config{SendPingTimeout: h2SendPingTimeout, PingTimeout: h2PingTimeout}
}

// newHealthCheckClient is the default client of a Manager: the standard
// transport with HTTP/2 pings. It has no client-wide Timeout, which would cap
// a target's own, longer healthcheck_timeout: every check runs under a context
// deadline, which bounds the response body as well as the headers.
func newHealthCheckClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.HTTP2 = http2WithPings()
	return &http.Client{Transport: t}
}

// connTrace records which connection a request went out on, so that a request
// that timed out can tell whether it hung on a connection taken from the pool
// (possibly a dead one) or on a freshly dialled one (the provider itself not
// answering).
type connTrace struct {
	mu   sync.Mutex
	got  bool
	info httptrace.GotConnInfo
}

// withConnTrace returns ctx with a trace that fills the returned connTrace.
// Hooks already in ctx keep firing: httptrace composes them.
func withConnTrace(ctx context.Context) (context.Context, *connTrace) {
	c := &connTrace{}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.got, c.info = true, info
		},
	}), c
}

// describe summarises the connection of the last attempt for an error message.
func (c *connTrace) describe() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !c.got:
		return "no connection obtained"
	case !c.info.Reused:
		return "new connection"
	case c.info.WasIdle:
		return fmt.Sprintf("reused connection, idle %s", c.info.IdleTime.Round(100*time.Millisecond))
	default:
		return "reused connection"
	}
}

// annotate appends the connection summary to err, keeping it unwrappable.
func (c *connTrace) annotate(err error) error {
	return fmt.Errorf("%w (%s)", err, c.describe())
}
