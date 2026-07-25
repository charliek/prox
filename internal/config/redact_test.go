package config

import (
	"testing"

	"github.com/charliek/prox/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureConfig_RedactEnabled(t *testing.T) {
	var nilCfg *CaptureConfig
	assert.False(t, nilCfg.RedactEnabled(), "nil config: no capture config, redaction off")

	assert.True(t, (&CaptureConfig{}).RedactEnabled(), "unset redact defaults on")

	tru, fls := true, false
	assert.True(t, (&CaptureConfig{Redact: &tru}).RedactEnabled(), "explicit true is on")
	assert.False(t, (&CaptureConfig{Redact: &fls}).RedactEnabled(), "explicit false disables")
}

// TestParse_CaptureRedaction covers parsing, defaulting, canonicalization, and
// de-duplication of the redaction config (plan 012 D4).
func TestParse_CaptureRedaction(t *testing.T) {
	t.Run("defaults on with canonicalized deduped lists", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  domain: local.dev
  https_port: 8443
  capture:
    enabled: true
    redact_headers:
      - x-trace-id
      - X-Trace-Id
      - x-secret
    redact_query_params:
      - SIG
      - sig
      - nonce
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)
		require.NotNil(t, cfg.Proxy.Capture)
		assert.True(t, cfg.Proxy.Capture.RedactEnabled(), "redact defaults on when capture config exists")
		// Canonicalized and de-duplicated (x-trace-id / X-Trace-Id collapse).
		assert.Equal(t, []string{"X-Trace-Id", "X-Secret"}, cfg.Proxy.Capture.RedactHeaders)
		// Lowercased and de-duplicated.
		assert.Equal(t, []string{"sig", "nonce"}, cfg.Proxy.Capture.RedactQueryParams)
	})

	t.Run("explicit redact false disables", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  domain: local.dev
  https_port: 8443
  capture:
    enabled: true
    redact: false
`
		cfg, err := Parse([]byte(yaml))
		require.NoError(t, err)
		assert.False(t, cfg.Proxy.Capture.RedactEnabled())
	})

	t.Run("invalid header name rejected", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  domain: local.dev
  https_port: 8443
  capture:
    enabled: true
    redact_headers:
      - "Bad Name"
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.ErrorIs(t, err, domain.ErrInvalidConfig)
		assert.Contains(t, err.Error(), "redact_headers")
	})

	t.Run("header name with colon rejected", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  domain: local.dev
  https_port: 8443
  capture:
    enabled: true
    redact_headers:
      - "X-Bad:"
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "redact_headers")
	})

	t.Run("empty query param rejected", func(t *testing.T) {
		yaml := `
processes:
  web: npm run dev

proxy:
  enabled: true
  domain: local.dev
  https_port: 8443
  capture:
    enabled: true
    redact_query_params:
      - ""
`
		_, err := Parse([]byte(yaml))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "redact_query_params")
	})
}
