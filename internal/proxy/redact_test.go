package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/config"
	"github.com/charliek/prox/internal/constants"
)

// boolPtr is a local helper for the *bool tri-state Redact config field.
func boolPtr(b bool) *bool { return &b }

// redactOnPolicy is a policy with redaction enabled and no extensions (built-ins
// only) — the common shape for the header/query unit tests.
func redactOnPolicy() CapturePolicy {
	return CapturePolicy{Redact: true}
}

func TestRedactHeadersBuiltins(t *testing.T) {
	p := redactOnPolicy()
	h := http.Header{
		"Authorization":       {"Bearer secret-token"},
		"Proxy-Authorization": {"Basic abc"},
		"Cookie":              {"session=deadbeef"},
		"Set-Cookie":          {"a=1", "b=2"},
		"X-Api-Key":           {"key-123"},
		"X-Auth-Token":        {"tok-456"},
		"Content-Type":        {"application/json"},
		"X-Custom":            {"keep-me"},
	}

	got := p.redactHeaders(h)

	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Auth-Token"} {
		assert.Equal(t, []string{headerRedactedValue}, got[name], "header %s value redacted", name)
	}
	// Repeated values in a sensitive header are each redacted.
	assert.Equal(t, []string{headerRedactedValue, headerRedactedValue}, got["Set-Cookie"])
	// Non-sensitive headers pass through untouched.
	assert.Equal(t, []string{"application/json"}, got["Content-Type"])
	assert.Equal(t, []string{"keep-me"}, got["X-Custom"])

	// Original header map and its value slices are never mutated.
	assert.Equal(t, []string{"Bearer secret-token"}, h["Authorization"])
	assert.Equal(t, []string{"a=1", "b=2"}, h["Set-Cookie"])
}

func TestRedactHeadersCaseInsensitive(t *testing.T) {
	p := redactOnPolicy()
	// The matcher normalizes names, so any case matches a built-in.
	for _, name := range []string{"authorization", "AUTHORIZATION", "Authorization", "cookie", "COOKIE", "x-api-key", "X-Api-KEY"} {
		assert.True(t, p.isRedactedHeader(name), "isRedactedHeader(%q) should match a built-in", name)
	}
	assert.False(t, p.isRedactedHeader("X-Public"))

	// End-to-end through a real http.Header (canonicalized on Set).
	h := http.Header{}
	h.Set("authorization", "Bearer x")
	h.Set("cookie", "s=1")
	got := p.redactHeaders(h)
	assert.Equal(t, headerRedactedValue, got.Get("Authorization"))
	assert.Equal(t, headerRedactedValue, got.Get("Cookie"))
}

func TestRedactHeadersExtensions(t *testing.T) {
	p := CapturePolicy{Redact: true, RedactHeaders: []string{"X-Custom-Token", "X-Secret"}}
	h := http.Header{
		"X-Custom-Token": {"abc"},
		"X-Secret":       {"shh"},
		"Authorization":  {"Bearer builtin-still-on"},
		"X-Public":       {"ok"},
	}
	got := p.redactHeaders(h)
	assert.Equal(t, []string{headerRedactedValue}, got["X-Custom-Token"])
	assert.Equal(t, []string{headerRedactedValue}, got["X-Secret"])
	// Extensions never disable the built-ins.
	assert.Equal(t, []string{headerRedactedValue}, got["Authorization"])
	assert.Equal(t, []string{"ok"}, got["X-Public"])
}

func TestRedactHeadersDisabled(t *testing.T) {
	p := CapturePolicy{Redact: false}
	h := http.Header{"Authorization": {"Bearer secret"}}
	got := p.redactHeaders(h)
	// Redaction off: full bypass, but still a clone (independent map).
	assert.Equal(t, []string{"Bearer secret"}, got["Authorization"])
	got.Set("Authorization", "mutated")
	assert.Equal(t, []string{"Bearer secret"}, h["Authorization"], "clone is independent of original")
}

func TestRedactHeadersNil(t *testing.T) {
	assert.Nil(t, redactOnPolicy().redactHeaders(nil))
}

func TestRedactQueryParams(t *testing.T) {
	p := redactOnPolicy()

	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"single builtin", "code=abc123", "code=" + queryRedactedValue},
		{"repeated params all redacted", "token=a&token=b", "token=" + queryRedactedValue + "&token=" + queryRedactedValue},
		{"non-sensitive preserved verbatim", "page=2&sort=asc", "page=2&sort=asc"},
		{
			"order preserved, only secrets redacted",
			"a=1&access_token=xyz&b=2&code=q",
			"a=1&access_token=" + queryRedactedValue + "&b=2&code=" + queryRedactedValue,
		},
		{"blank value", "code=", "code=" + queryRedactedValue},
		{"bare key no value left alone", "code", "code"},
		{"mixed case name matches", "CODE=abc", "CODE=" + queryRedactedValue},
		{"percent-encoded name matches", "%63ode=abc", "%63ode=" + queryRedactedValue},
		{"value containing encoded ampersand redacted whole", "code=a%26b%3Dc", "code=" + queryRedactedValue},
		{"all builtin names", "access_token=1&refresh_token=2&id_token=3&api_key=4&apikey=5&client_secret=6", "access_token=" + queryRedactedValue + "&refresh_token=" + queryRedactedValue + "&id_token=" + queryRedactedValue + "&api_key=" + queryRedactedValue + "&apikey=" + queryRedactedValue + "&client_secret=" + queryRedactedValue},
		{"empty query unchanged", "", ""},
		{"malformed name skipped, valid sibling still redacted", "%zz&code=secret", "%zz&code=" + queryRedactedValue},
		{"fully malformed left as-is", "%zz=%yy", "%zz=%yy"},
		// Separator handling (leak path 1): ';' is a valid pair delimiter.
		{"semicolon separator", "a=1;code=SECRET", "a=1;code=" + queryRedactedValue},
		{"semicolon in the middle", "code=x;b=2", "code=" + queryRedactedValue + ";b=2"},
		{"mixed separators preserved byte-exact", "a=1;b=2&code=x&d=4", "a=1;b=2&code=" + queryRedactedValue + "&d=4"},
		{"semicolon inside non-redacted value survives", "a=1;2", "a=1;2"},
		{"semicolon around redacted param preserved", "x=1;token=t;y=2", "x=1;token=" + queryRedactedValue + ";y=2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, p.redactRawQuery(tc.query))
		})
	}
}

func TestRedactQueryParamExtensions(t *testing.T) {
	p := CapturePolicy{Redact: true, RedactQueryParams: []string{"sig", "signature"}}
	assert.Equal(t, "sig="+queryRedactedValue+"&token="+queryRedactedValue,
		p.redactRawQuery("sig=abc&token=xyz"), "extension and builtin both redacted")
	assert.Equal(t, "other=keep", p.redactRawQuery("other=keep"))
}

func TestRedactURLStringDoesNotMutate(t *testing.T) {
	p := redactOnPolicy()
	u, err := url.Parse("https://api.local.dev/callback?state=ok&code=SECRET&next=/home")
	require.NoError(t, err)
	original := u.String()

	got := p.RedactURLString(u)
	assert.Equal(t, "https://api.local.dev/callback?state=ok&code="+queryRedactedValue+"&next=/home", got)
	// u is untouched: the forwarded upstream URL stays byte-identical.
	assert.Equal(t, original, u.String())
	assert.Equal(t, "state=ok&code=SECRET&next=/home", u.RawQuery)
}

func TestRedactURLStringNoQueryOrDisabled(t *testing.T) {
	u, _ := url.Parse("https://api.local.dev/path?code=x")

	off := CapturePolicy{Redact: false}
	assert.Equal(t, u.String(), off.RedactURLString(u), "redaction off is a pure passthrough")

	noQuery, _ := url.Parse("https://api.local.dev/path")
	assert.Equal(t, noQuery.String(), redactOnPolicy().RedactURLString(noQuery))
}

func TestRedactLocationRefererHeaders(t *testing.T) {
	p := redactOnPolicy()

	t.Run("absolute Location with oauth code", func(t *testing.T) {
		h := http.Header{"Location": {"https://app.example.com/cb?code=AUTHCODE&state=xyz"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"https://app.example.com/cb?code=" + queryRedactedValue + "&state=xyz"}, got["Location"])
	})

	t.Run("relative Location", func(t *testing.T) {
		h := http.Header{"Location": {"/callback?code=abc&ok=1"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"/callback?code=" + queryRedactedValue + "&ok=1"}, got["Location"])
	})

	t.Run("Referer redacted too", func(t *testing.T) {
		h := http.Header{"Referer": {"https://id.example.com/authorize?access_token=leak"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"https://id.example.com/authorize?access_token=" + queryRedactedValue}, got["Referer"])
	})

	t.Run("Location with fragment preserves fragment", func(t *testing.T) {
		h := http.Header{"Location": {"/cb?code=abc#section"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"/cb?code=" + queryRedactedValue + "#section"}, got["Location"])
	})

	t.Run("Location without query untouched", func(t *testing.T) {
		h := http.Header{"Location": {"/dashboard"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"/dashboard"}, got["Location"])
	})

	t.Run("unparseable garbage left verbatim", func(t *testing.T) {
		garbage := "://::: not a url at all"
		h := http.Header{"Location": {garbage}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{garbage}, got["Location"])
	})

	t.Run("Location with no sensitive params unchanged", func(t *testing.T) {
		h := http.Header{"Location": {"/next?page=2"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"/next?page=2"}, got["Location"])
	})

	// Leak path 2: an unparseable Location (invalid percent-escape in the path)
	// must still have its query redacted — the fail-closed string-surgery path,
	// not url.Parse-then-verbatim.
	t.Run("invalid percent-escape Location still redacted", func(t *testing.T) {
		require.Error(t, urlParseErr("/cb%ZZ?code=SECRET&state=1"), "precondition: value must be unparseable")
		h := http.Header{"Location": {"/cb%ZZ?code=SECRET&state=1"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"/cb%ZZ?code=" + queryRedactedValue + "&state=1"}, got["Location"])
	})

	// Leak path 3: implicit-flow tokens ride in the fragment, not the query.
	t.Run("fragment-only token redacted", func(t *testing.T) {
		h := http.Header{"Location": {"https://app/cb#access_token=LEAK&token_type=bearer"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"https://app/cb#access_token=" + queryRedactedValue + "&token_type=bearer"}, got["Location"])
	})

	t.Run("query and fragment both redacted", func(t *testing.T) {
		h := http.Header{"Location": {"https://app/cb?code=Q#access_token=F&state=1"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"https://app/cb?code=" + queryRedactedValue + "#access_token=" + queryRedactedValue + "&state=1"}, got["Location"])
	})

	// Leak path 4: userinfo password in an absolute Location.
	t.Run("userinfo password redacted, username kept", func(t *testing.T) {
		h := http.Header{"Location": {"https://user:s3cr3t@host/path?code=x"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"https://user:" + queryRedactedValue + "@host/path?code=" + queryRedactedValue}, got["Location"])
	})

	t.Run("userinfo without password left alone", func(t *testing.T) {
		h := http.Header{"Location": {"https://user@host/path"}}
		got := p.redactHeaders(h)
		assert.Equal(t, []string{"https://user@host/path"}, got["Location"])
	})
}

// urlParseErr returns the error (if any) from parsing s, for asserting that a
// crafted value is genuinely unparseable.
func urlParseErr(s string) error {
	_, err := url.Parse(s)
	return err
}

// TestRedactURLStringLeakPaths pins the stored-record-URL redaction for the
// fragment and userinfo-password leak paths (plan 012 D4), applied defensively
// even though a server-side r.URL rarely carries either.
func TestRedactURLStringLeakPaths(t *testing.T) {
	p := redactOnPolicy()

	t.Run("fragment params redacted", func(t *testing.T) {
		u, err := url.Parse("https://api.local.dev/cb?state=1#access_token=LEAK&x=2")
		require.NoError(t, err)
		got := p.RedactURLString(u)
		assert.Equal(t, "https://api.local.dev/cb?state=1#access_token="+queryRedactedValue+"&x=2", got)
		assert.Equal(t, "access_token=LEAK&x=2", u.Fragment, "source URL untouched")
	})

	t.Run("userinfo password redacted", func(t *testing.T) {
		u, err := url.Parse("https://user:pass@api.local.dev/path?code=x")
		require.NoError(t, err)
		got := p.RedactURLString(u)
		assert.Equal(t, "https://user:"+queryRedactedValue+"@api.local.dev/path?code="+queryRedactedValue, got)
		pw, _ := u.User.Password()
		assert.Equal(t, "pass", pw, "source URL userinfo untouched")
	})

	t.Run("semicolon-separated query redacted", func(t *testing.T) {
		u, err := url.Parse("https://api.local.dev/x?a=1;code=SECRET")
		require.NoError(t, err)
		got := p.RedactURLString(u)
		assert.Equal(t, "https://api.local.dev/x?a=1;code="+queryRedactedValue, got)
	})
}

func TestCapturePolicyFromCaptureConfig(t *testing.T) {
	t.Run("nil config is redaction off", func(t *testing.T) {
		p := CapturePolicyFromCaptureConfig(nil)
		assert.False(t, p.Redact)
	})

	t.Run("default (redact unset) is on", func(t *testing.T) {
		p := CapturePolicyFromCaptureConfig(&config.CaptureConfig{Enabled: true})
		assert.True(t, p.Redact)
	})

	t.Run("explicit false disables", func(t *testing.T) {
		p := CapturePolicyFromCaptureConfig(&config.CaptureConfig{Enabled: true, Redact: boolPtr(false)})
		assert.False(t, p.Redact)
	})

	t.Run("extensions copied, not aliased", func(t *testing.T) {
		src := []string{"X-Trace"}
		cfg := &config.CaptureConfig{Enabled: true, RedactHeaders: src}
		p := CapturePolicyFromCaptureConfig(cfg)
		require.Equal(t, []string{"X-Trace"}, p.RedactHeaders)
		src[0] = "mutated"
		assert.Equal(t, []string{"X-Trace"}, p.RedactHeaders, "policy slice independent of config slice")
	})
}

// TestCaptureRequestBodylessRedactsHeaders is the panel's critical finding: a
// bodyless GET/HEAD (r.Body == nil) still reaches the stored Details, so its
// headers MUST be redacted by CaptureRequest even though there is no body to
// capture (plan 012 D4).
func TestCaptureRequestBodylessRedactsHeaders(t *testing.T) {
	cm := newEnabledCaptureManager(t)

	req := httptest.NewRequest("GET", "/x", nil)
	req.Body = nil // httptest sets http.NoBody; a truly bodyless request nils it
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("X-Public", "fine")

	body, wrapped, headers := cm.CaptureRequest("get1", req, redactOnPolicy())
	assert.Nil(t, body)
	assert.Nil(t, wrapped)
	assert.Equal(t, headerRedactedValue, headers.Get("Authorization"))
	assert.Equal(t, headerRedactedValue, headers.Get("Cookie"))
	assert.Equal(t, "fine", headers.Get("X-Public"))
}

// TestCaptureRequestWithBodyRedactsHeaders covers the request-with-body entry
// point (plan 012 D4 structural coverage): no RequestDetails carries an
// unredacted built-in header through either capture entry point.
func TestCaptureRequestWithBodyRedactsHeaders(t *testing.T) {
	cm := newEnabledCaptureManager(t)

	req := httptest.NewRequest("POST", "/x", strings.NewReader("payload"))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Api-Key", "k")

	_, wrapped, headers := cm.CaptureRequest("post1", req, redactOnPolicy())
	_, _ = io.ReadAll(wrapped)
	require.NoError(t, wrapped.Close())
	assert.Equal(t, headerRedactedValue, headers.Get("Authorization"))
	assert.Equal(t, headerRedactedValue, headers.Get("X-Api-Key"))
}

// TestFinalizeResponseRedactsHeaders covers the response entry point, including
// the Set-Cookie built-in and an oauth code inside a redirect Location.
func TestFinalizeResponseRedactsHeaders(t *testing.T) {
	cm := newEnabledCaptureManager(t)

	crw := newCaptureResponseWriter(httptest.NewRecorder(), constants.DefaultCaptureMaxBodySize)
	crw.Header().Set("Set-Cookie", "sid=secret")
	crw.Header().Set("Location", "https://app.example.com/cb?code=LEAK&state=1")
	crw.Header().Set("Content-Type", "text/html")
	crw.WriteHeader(http.StatusFound)

	_, headers := cm.FinalizeResponse("resp1", crw, redactOnPolicy())
	assert.Equal(t, headerRedactedValue, headers.Get("Set-Cookie"))
	assert.Equal(t, "https://app.example.com/cb?code="+queryRedactedValue+"&state=1", headers.Get("Location"))
	assert.Equal(t, "text/html", headers.Get("Content-Type"))
}
