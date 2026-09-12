package domain

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeHubURL validates a hub's control-plane URL and returns its
// canonical scheme://host[/path] form with any trailing slash removed (plan
// 031 D8/D16).
//
// It lives in domain because BOTH sides of the hub need exactly this rule and
// neither package may import the other: internal/config validates a project's
// `hubs:` block and `prox hub add`'s argument before writing
// ~/.prox/hubs.yaml, while internal/proxyd validates the same string again at
// NewHTTPClient time, before a bearer token is ever attached to it. A copy in
// each package would be two rules that drift; domain is the one place both
// already depend on.
//
// The rejections are deliberate rather than tolerant. A query or fragment
// would be silently dropped the moment a client appends its own path and
// query, and userinfo would put a second credential on a request that already
// carries a bearer token. Failing here turns each into a config error the user
// can see, instead of a request that quietly goes somewhere else.
//
// Error texts are written to read correctly after a caller-supplied subject,
// since the two callers name the offender differently ("hubs.llt.url: ..." in
// a config report, "hub url %q: ..." from the client). They therefore do not
// repeat the URL themselves.
//
// The returned string is what callers should store and build requests against;
// a caller that only needs the verdict can discard it.
func NormalizeHubURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("is empty")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("is not a valid url: %w", err)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("must be an absolute http:// or https:// url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("must use http:// or https:// (got %q)", u.Scheme)
	}
	// Hostname(), not Host: "http://:8443" carries a Host of ":8443" and would
	// pass a Host != "" check, yet names no host at all — a request built
	// against it dials an unspecified address rather than the hub (plan 031).
	if u.Hostname() == "" {
		return "", fmt.Errorf("has no host")
	}
	if u.User != nil {
		return "", fmt.Errorf("must not contain userinfo (use a token instead)")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("must not contain a query string")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return "", fmt.Errorf("must not contain a fragment")
	}

	// Normalize the trailing slash (and any run of them) so a caller can
	// append "/api/v1/..." without producing a doubled separator.
	path := strings.TrimRight(u.EscapedPath(), "/")
	return u.Scheme + "://" + u.Host + path, nil
}
