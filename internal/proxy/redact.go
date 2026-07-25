package proxy

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/charliek/prox/internal/config"
)

// Redaction placeholder values (plan 012 D4). Headers keep the bracketed form so
// a redacted value is unmistakable in the captured record; query params use the
// bare token because it sits inside a URL, where brackets would be percent-
// encoding noise.
const (
	headerRedactedValue = "[REDACTED]"
	queryRedactedValue  = "REDACTED"
)

// builtinRedactHeaders is the always-on set of sensitive header names (plan 012
// D4), stored in canonical (http.CanonicalHeaderKey) form for direct lookup.
// Project config extends — never replaces — this set.
var builtinRedactHeaders = map[string]struct{}{
	"Authorization":       {},
	"Proxy-Authorization": {},
	"Cookie":              {},
	"Set-Cookie":          {},
	"X-Api-Key":           {},
	"X-Auth-Token":        {},
}

// builtinRedactQueryParams is the always-on set of sensitive query-parameter
// names (plan 012 D4), stored lowercased for case-insensitive matching. Project
// config extends — never replaces — this set.
var builtinRedactQueryParams = map[string]struct{}{
	"access_token":  {},
	"refresh_token": {},
	"id_token":      {},
	"token":         {},
	"api_key":       {},
	"apikey":        {},
	"client_secret": {},
	"code":          {},
}

// urlBearingHeaders are the header names whose VALUE is a URL that may itself
// carry a sensitive query param — the OAuth-code-in-302-Location leak (plan 012
// D4). Their query params are redacted in place while the rest of the value is
// preserved. Canonical form for direct lookup.
var urlBearingHeaders = map[string]struct{}{
	"Location": {},
	"Referer":  {},
}

// CapturePolicy is the per-call capture configuration threaded through every
// capture entry point (plan 012 D4). The daemon builds one per request from the
// matched route's fields; the standalone proxy builds one from its project
// config. It absorbs the former per-call MaxBodySize limit (D13, #49) so one
// value carries both the body cap and the redaction policy.
//
// RedactHeaders/RedactQueryParams EXTEND the built-in sets (never replace them).
// Entries are expected canonicalized (headers) / lowercased (query params) at
// config parse time, but every match normalizes defensively so a policy built by
// hand still behaves.
type CapturePolicy struct {
	MaxBodySize       int64
	Redact            bool
	RedactHeaders     []string
	RedactQueryParams []string
}

// CapturePolicyFromCaptureConfig builds the standalone proxy's per-call policy
// from its project capture config (plan 012 D4). A nil config (or one with
// redaction disabled) yields Redact=false. MaxBodySize is left 0 so the capture
// manager's own configured cap (derived from the same config) applies — the
// standalone path never overrides its manager's limit.
func CapturePolicyFromCaptureConfig(c *config.CaptureConfig) CapturePolicy {
	if c == nil {
		return CapturePolicy{}
	}
	return CapturePolicy{
		Redact:            c.RedactEnabled(),
		RedactHeaders:     append([]string(nil), c.RedactHeaders...),
		RedactQueryParams: append([]string(nil), c.RedactQueryParams...),
	}
}

// isRedactedHeader reports whether a header name is a sensitive one whose value
// must be replaced wholesale. Matches the built-ins and the policy's extensions,
// both compared in canonical form (case-insensitive).
func (p CapturePolicy) isRedactedHeader(name string) bool {
	canon := http.CanonicalHeaderKey(name)
	if _, ok := builtinRedactHeaders[canon]; ok {
		return true
	}
	for _, h := range p.RedactHeaders {
		if http.CanonicalHeaderKey(h) == canon {
			return true
		}
	}
	return false
}

// isURLBearingHeader reports whether a header carries a URL value whose query
// params should be redacted in place (Location/Referer).
func (p CapturePolicy) isURLBearingHeader(name string) bool {
	_, ok := urlBearingHeaders[http.CanonicalHeaderKey(name)]
	return ok
}

// isRedactedQueryParam reports whether a query-param name is sensitive. Matches
// the built-ins and the policy's extensions, both compared lowercased.
func (p CapturePolicy) isRedactedQueryParam(name string) bool {
	lower := strings.ToLower(name)
	if _, ok := builtinRedactQueryParams[lower]; ok {
		return true
	}
	for _, q := range p.RedactQueryParams {
		if strings.ToLower(q) == lower {
			return true
		}
	}
	return false
}

// redactHeaders clones h and, when redaction is enabled, replaces every
// sensitive header value with [REDACTED] and rewrites sensitive query params
// inside URL-bearing header values (Location/Referer) in place (plan 012 D4).
// The clone is always returned (callers store it directly). The original header
// map and its value slices are never mutated: redaction only ever REPLACES a
// clone entry with a freshly allocated slice, and cloneHeaders shares the value
// slices with the original, so in-place element writes are deliberately avoided.
// Only initial headers are captured (trailers are never captured anywhere), so
// trailer redaction is out of scope.
func (p CapturePolicy) redactHeaders(h http.Header) http.Header {
	clone := cloneHeaders(h)
	if clone == nil || !p.Redact {
		return clone
	}
	for name, vals := range clone {
		switch {
		case p.isRedactedHeader(name):
			red := make([]string, len(vals))
			for i := range red {
				red[i] = headerRedactedValue
			}
			clone[name] = red
		case p.isURLBearingHeader(name):
			red := make([]string, len(vals))
			for i, v := range vals {
				red[i] = p.redactURLValue(v)
			}
			clone[name] = red
		}
	}
	return clone
}

// redactRawQuery rewrites sensitive query-param VALUES to REDACTED directly in a
// raw query (or query-formatted fragment) string (plan 012 D4), preserving
// parameter order and every non-redacted byte verbatim — it deliberately does
// NOT round-trip through url.Values.Encode, which reorders and re-encodes.
//
// Pairs are delimited by EITHER '&' or ';' and the actual delimiter is preserved
// byte-for-byte (a ';'-separated query such as "a=1;code=x" is a real leak path
// that a '&'-only split would miss). Repeated sensitive params are all redacted;
// a bare key (no '=') carries no value and is left alone. A param whose name
// cannot be percent-decoded is skipped, so a malformed fragment never blocks
// redaction of a valid sibling and never errors (per-part resilience). Returns
// the input unchanged (same bytes) when nothing matched.
func (p CapturePolicy) redactRawQuery(rawQuery string) string {
	if rawQuery == "" {
		return rawQuery
	}
	var b strings.Builder
	changed := false
	start := 0
	for i := 0; i <= len(rawQuery); i++ {
		if i < len(rawQuery) && rawQuery[i] != '&' && rawQuery[i] != ';' {
			continue
		}
		seg, segChanged := p.redactQuerySegment(rawQuery[start:i])
		b.WriteString(seg)
		changed = changed || segChanged
		if i < len(rawQuery) {
			b.WriteByte(rawQuery[i]) // preserve the actual '&' or ';' delimiter
		}
		start = i + 1
	}
	if !changed {
		return rawQuery
	}
	return b.String()
}

// redactQuerySegment redacts one key=value pair's value if the key is sensitive,
// returning the (possibly rewritten) segment and whether it changed. The raw key
// bytes are preserved (only the value becomes REDACTED).
func (p CapturePolicy) redactQuerySegment(seg string) (string, bool) {
	eq := strings.IndexByte(seg, '=')
	rawKey := seg
	if eq >= 0 {
		rawKey = seg[:eq]
	}
	key, err := url.QueryUnescape(rawKey)
	if err != nil || eq < 0 || !p.isRedactedQueryParam(key) {
		return seg, false
	}
	return rawKey + "=" + queryRedactedValue, true
}

// RedactURLString returns u.String() with sensitive query params, fragment
// params, and any userinfo password redacted (plan 012 D4). It NEVER mutates u —
// the URL the reverse proxy forwards upstream must stay byte-identical — because
// u.String() produces a fresh string that redactURLLike then rewrites in place.
// When redaction is off, or nothing matched, the result is exactly u.String().
func (p CapturePolicy) RedactURLString(u *url.URL) string {
	if u == nil {
		return ""
	}
	if !p.Redact {
		return u.String()
	}
	return p.redactURLLike(u.String())
}

// redactURLValue redacts sensitive query/fragment params and any userinfo
// password inside a URL-valued header (Location/Referer) — the OAuth-code and
// implicit-flow-token leak fixes (plan 012 D4). It is fail-CLOSED: it never
// depends on url.Parse succeeding (crafted invalid percent-escapes are exactly
// the values an attacker would use), operating purely with string surgery so the
// query and fragment are redacted regardless of parseability.
func (p CapturePolicy) redactURLValue(v string) string {
	return p.redactURLLike(v)
}

// redactURLLike redacts a URL-shaped string via string surgery (plan 012 D4),
// fail-closed and byte-preserving. It splits off the fragment at the first '#'
// and the query at the first '?' (before the fragment), redacts sensitive params
// in BOTH the query and the query-formatted fragment (implicit-flow tokens live
// in the fragment), and redacts any userinfo password in the authority. Every
// non-redacted byte and delimiter survives verbatim; a value with no query, no
// fragment, and no userinfo password passes through unchanged.
func (p CapturePolicy) redactURLLike(v string) string {
	base := v
	frag := ""
	hasFrag := false
	if h := strings.IndexByte(v, '#'); h >= 0 {
		base = v[:h]
		frag = v[h+1:]
		hasFrag = true
	}
	pre := base
	query := ""
	hasQuery := false
	if q := strings.IndexByte(base, '?'); q >= 0 {
		pre = base[:q]
		query = base[q+1:]
		hasQuery = true
	}

	newPre := p.redactUserinfo(pre)
	newQuery := query
	if hasQuery {
		newQuery = p.redactRawQuery(query)
	}
	newFrag := frag
	if hasFrag {
		newFrag = p.redactRawQuery(frag)
	}

	if newPre == pre && newQuery == query && newFrag == frag {
		return v // byte-identical passthrough
	}

	var b strings.Builder
	b.WriteString(newPre)
	if hasQuery {
		b.WriteByte('?')
		b.WriteString(newQuery)
	}
	if hasFrag {
		b.WriteByte('#')
		b.WriteString(newFrag)
	}
	return b.String()
}

// redactUserinfo replaces the password portion of a URL authority's userinfo
// with REDACTED, keeping the username (plan 012 D4): "https://user:pass@host/p"
// becomes "https://user:REDACTED@host/p". A userinfo with no password (or a
// value with no "//" authority, e.g. a relative Location) is returned unchanged.
// pre must already have its query and fragment stripped. Everything outside the
// password span survives byte-for-byte.
func (p CapturePolicy) redactUserinfo(pre string) string {
	i := strings.Index(pre, "//")
	if i < 0 {
		return pre // no authority (relative reference)
	}
	authStart := i + 2
	authEnd := len(pre)
	if s := strings.IndexByte(pre[authStart:], '/'); s >= 0 {
		authEnd = authStart + s
	}
	authority := pre[authStart:authEnd]
	// The last '@' delimits userinfo from host ('@' inside userinfo is %-encoded).
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return pre // no userinfo
	}
	colon := strings.IndexByte(authority[:at], ':')
	if colon < 0 {
		return pre // username only, no password to redact
	}
	pwStart := authStart + colon + 1
	pwEnd := authStart + at
	return pre[:pwStart] + queryRedactedValue + pre[pwEnd:]
}
