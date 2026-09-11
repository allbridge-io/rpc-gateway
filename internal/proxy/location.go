package proxy

import (
	"context"
	"net/url"
	"strings"
)

// clientPrefixKey carries the path prefix the router stripped from the
// client's URL before handing the request to the proxy: "/{api-key}/{chain}"
// or just "/{chain}" without configured keys.
type clientPrefixKey struct{}

// WithClientPrefix records the prefix the client used to reach this chain, so
// that a redirect coming back from the target can be rewritten to stay behind
// it (see rewriteLocation).
func WithClientPrefix(ctx context.Context, prefix string) context.Context {
	return context.WithValue(ctx, clientPrefixKey{}, prefix)
}

// ClientPrefix returns the prefix recorded with WithClientPrefix, "" if none.
func ClientPrefix(ctx context.Context) string {
	prefix, _ := ctx.Value(clientPrefixKey{}).(string)
	return prefix
}

// rewriteLocation maps a Location header sent by the target back into the
// gateway's URL space. A target answering "301 Location: /extended" (Hiro does
// that for its root) would otherwise send a redirect-following client to
// https://gateway/extended: the API key and the chain are gone from the path
// and the client ends up with a 401 it did not cause.
//
// Only URLs the target owns are rewritten: a path-absolute one, or an absolute
// one on the target's own host. Both are mapped to a path-absolute URL under
// prefix (the client resolves it against the gateway, whose public scheme and
// host the gateway does not need to know), keeping query and fragment. A path
// outside the target's base path, a relative reference (resolved by the client
// against a URL that already carries the prefix) and a redirect to another host
// are left alone. The second return value says whether loc was rewritten.
func rewriteLocation(loc string, target *url.URL, prefix string) (string, bool) {
	if loc == "" || prefix == "" || target == nil {
		return "", false
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", false
	}
	switch {
	case u.Scheme != "" || u.Host != "":
		if !strings.EqualFold(u.Host, target.Host) {
			return "", false
		}
	case !strings.HasPrefix(u.Path, "/"):
		return "", false
	}
	base := strings.TrimSuffix(target.Path, "/")
	rest, ok := strings.CutPrefix(u.Path, base)
	if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
		return "", false
	}
	out := url.URL{Path: prefix + rest, RawQuery: u.RawQuery, Fragment: u.Fragment}
	return out.String(), true
}
