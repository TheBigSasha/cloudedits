package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/thebigsasha/cloudedits/internal/auth"
)

// originPolicy is the allowlist of browser origins permitted to call the API.
//
// The server authenticates callers by network position (loopback, tailnet
// identity), which a browser attaches to every request it makes regardless of
// which page asked for it. Any web page the owner visits could therefore act
// with the owner's authority unless requests carrying a foreign Origin are
// refused outright. Refusing (rather than merely omitting CORS headers) also
// stops preflight-free cross-site writes such as a text/plain POST.
type originPolicy struct {
	exact    map[string]bool
	wildcard []wildcardOrigin
	any      bool
}

type wildcardOrigin struct {
	scheme     string
	hostSuffix string // ".example.com"
}

// canonicalOrigin normalises an origin or base URL to "scheme://host[:port]"
// (lower case, default port removed). It rejects anything that is not an
// absolute http(s) origin, including the opaque "null" origin.
func canonicalOrigin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.ContainsAny(host, "* ") {
		return "", false
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, true
}

// newOriginPolicy builds the allowlist. Each entry is an origin or base URL
// ("https://hdr.toys"), a subdomain wildcard ("https://*.example.com"), or
// "*" to disable origin enforcement entirely. Empty entries are skipped;
// malformed entries fail construction so a typo never silently widens access.
func newOriginPolicy(entries ...string) (*originPolicy, error) {
	p := &originPolicy{exact: map[string]bool{}}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		switch entry {
		case "":
			continue
		case "*":
			p.any = true
			continue
		}
		if scheme, rest, ok := strings.Cut(entry, "://*."); ok {
			probe, valid := canonicalOrigin(scheme + "://wildcard." + rest)
			if !valid || rest == "" || strings.HasPrefix(rest, ".") {
				return nil, fmt.Errorf("invalid allowed origin %q", entry)
			}
			_, host, _ := strings.Cut(probe, "://wildcard")
			p.wildcard = append(p.wildcard, wildcardOrigin{scheme: strings.ToLower(scheme), hostSuffix: host})
			continue
		}
		canon, ok := canonicalOrigin(entry)
		if !ok {
			return nil, fmt.Errorf("invalid allowed origin %q", entry)
		}
		p.exact[canon] = true
	}
	return p, nil
}

// allows reports whether a request carrying the given Origin header may be
// served. Requests without an Origin header (non-browser clients, same-origin
// GETs, top-level navigations) are not subject to the policy.
func (p *originPolicy) allows(r *http.Request, origin string) bool {
	if origin == "" || p.any {
		return true
	}
	canon, ok := canonicalOrigin(origin)
	if !ok {
		return false
	}
	if p.exact[canon] {
		return true
	}
	scheme, host, _ := strings.Cut(canon, "://")
	for _, w := range p.wildcard {
		if scheme == w.scheme && strings.HasSuffix(host, w.hostSuffix) && len(host) > len(w.hostSuffix) {
			return true
		}
	}
	// A page served from this machine may reach a server on this machine: the
	// local development editor talking to a loopback listener. This never
	// applies to requests arriving over the network.
	return auth.IsLoopbackHost(host) && auth.IsTrustedLoopbackRequest(r)
}
