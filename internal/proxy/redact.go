package proxy

import (
	"net/url"
	"sort"
	"strings"

	"github.com/0xProject/rpc-gateway/internal/config"
)

// redactor replaces target URLs (which often carry API keys in the path or
// query) with the target's name wherever an error message is about to leave
// the process: /status, log events, taint reasons. Client errors already have
// their URL stripped in chaintype; this is the belt for anything else, such as
// a provider echoing the request URL in an error body.
type redactor struct {
	replacer *strings.Replacer
}

func newRedactor(targets []config.Target) *redactor {
	type pair struct{ secret, alias string }
	var pairs []pair
	add := func(raw, name string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		alias := "<" + name + ">"
		// Every form the URL can take in a message: as configured, without the
		// query (pass-through requests append the client's path before the
		// query, so the base URL no longer appears contiguously), the query
		// alone (an "api-key=..." pair on its own), and with the ws(s) scheme
		// rewritten to http(s) the way the WebSocket proxy dials it.
		forms := []string{raw, strings.TrimRight(raw, "/")}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			base := u.Scheme + "://" + u.Host + u.Path
			forms = append(forms, base, strings.TrimRight(base, "/"))
			if u.RawQuery != "" {
				pairs = append(pairs, pair{u.RawQuery, alias + "?"})
			}
		}
		for _, f := range forms {
			if f == "" {
				continue
			}
			pairs = append(pairs, pair{f, alias})
			switch {
			case strings.HasPrefix(f, "wss://"):
				pairs = append(pairs, pair{"https://" + strings.TrimPrefix(f, "wss://"), alias})
			case strings.HasPrefix(f, "ws://"):
				pairs = append(pairs, pair{"http://" + strings.TrimPrefix(f, "ws://"), alias})
			}
		}
	}
	for _, t := range targets {
		add(t.HTTPURL, t.Name)
		add(t.WSURL, t.Name)
	}
	// Longest first, so "https://host/v2/key" wins over "https://host".
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].secret) > len(pairs[j].secret) })
	args := make([]string, 0, 2*len(pairs))
	for _, p := range pairs {
		args = append(args, p.secret, p.alias)
	}
	return &redactor{replacer: strings.NewReplacer(args...)}
}

// String redacts a message.
func (r *redactor) String(s string) string {
	if r == nil || r.replacer == nil {
		return s
	}
	return r.replacer.Replace(s)
}

// Error redacts an error's text while keeping the original in the chain, so
// errors.Is / errors.As keep working (context.Canceled, *upstreamError...).
func (r *redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	msg := r.String(err.Error())
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, cause: err}
}

type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }
