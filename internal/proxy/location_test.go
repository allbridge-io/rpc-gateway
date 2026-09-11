package proxy

import (
	"net/url"
	"testing"
)

func TestRewriteLocation(t *testing.T) {
	hiro, _ := url.Parse("https://api.hiro.so")
	keyed, _ := url.Parse("https://tron.example/v2/secret-key/")
	const prefix = "/0123456789abcdef/STX"

	tests := []struct {
		name   string
		loc    string
		target *url.URL
		prefix string
		want   string
		ok     bool
	}{
		{"path-absolute (what Hiro sends for /)", "/extended", hiro, prefix, prefix + "/extended", true},
		{"root", "/", hiro, prefix, prefix + "/", true},
		{"query and fragment kept", "/extended/v1/block?limit=1#top", hiro, prefix, prefix + "/extended/v1/block?limit=1#top", true},
		{"absolute on the target's host", "https://api.hiro.so/extended", hiro, prefix, prefix + "/extended", true},
		{"absolute, host compared case-insensitively", "https://API.HIRO.SO/extended", hiro, prefix, prefix + "/extended", true},
		{"absolute to another host is not ours", "https://docs.hiro.so/", hiro, prefix, "", false},
		{"relative reference resolves against the prefixed URL already", "extended", hiro, prefix, "", false},
		{"target base path stripped", "/v2/secret-key/wallet/x?y=1", keyed, "/k/TRX", "/k/TRX/wallet/x?y=1", true},
		{"target base path itself", "/v2/secret-key", keyed, "/k/TRX", "/k/TRX", true},
		{"outside the target base path", "/other/wallet/x", keyed, "/k/TRX", "", false},
		{"base path prefix of a longer segment", "/v2/secret-key2/x", keyed, "/k/TRX", "", false},
		{"no prefix recorded", "/extended", hiro, "", "", false},
		{"empty location", "", hiro, prefix, "", false},
		{"unparsable location", "http://[::1", hiro, prefix, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := rewriteLocation(tt.loc, tt.target, tt.prefix)
			if ok != tt.ok || got != tt.want {
				t.Errorf("rewriteLocation(%q) = %q, %v; want %q, %v", tt.loc, got, ok, tt.want, tt.ok)
			}
		})
	}
}
