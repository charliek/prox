package domain

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateHost_EnforcesDNSLimits is plan 031, review B6.
//
// The hostname regex says what characters a host may contain and nothing about
// how many, so a megabyte of perfectly valid characters was a valid "host". On
// the hub's network mount that string arrives from another machine, is stored
// as a route target, and is then composed into a CONNECT preamble that can
// never fit the tunnel's 512-byte framing — so every dial through it fails,
// with the registration long since accepted.
func TestValidateHost_EnforcesDNSLimits(t *testing.T) {
	t.Run("a host at the limit is accepted", func(t *testing.T) {
		// 253 characters made of 63-character labels: "a"*63 . "a"*63 . … so
		// both limits are exercised at their boundary rather than past it.
		label := strings.Repeat("a", 63)
		host := strings.Join([]string{label, label, label, strings.Repeat("b", 61)}, ".")
		require.Len(t, host, 253)
		assert.NoError(t, ValidateHost(host))
	})

	t.Run("a host past 253 characters is refused", func(t *testing.T) {
		label := strings.Repeat("a", 63)
		host := strings.Join([]string{label, label, label, label}, ".")
		require.Greater(t, len(host), 253)
		err := ValidateHost(host)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too long")
	})

	t.Run("a LABEL past 63 characters is refused", func(t *testing.T) {
		err := ValidateHost(strings.Repeat("a", 64) + ".example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "label")
	})

	t.Run("an enormous host is refused rather than stored", func(t *testing.T) {
		err := ValidateHost(strings.Repeat("a", 1<<20))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too long")
	})

	t.Run("ordinary hosts and IP literals are unaffected", func(t *testing.T) {
		for _, host := range []string{"localhost", "127.0.0.1", "::1", "api.internal", "db-1.example.com"} {
			assert.NoError(t, ValidateHost(host), host)
		}
	})
}
