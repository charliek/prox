package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDedupeWarnings pins the identity rule: same Code AND Message collapses,
// same Code with a different Message does not, and first-seen order survives.
func TestDedupeWarnings(t *testing.T) {
	t.Run("nil and empty are safe", func(t *testing.T) {
		assert.Nil(t, DedupeWarnings(nil))
		assert.Nil(t, DedupeWarnings([]Warning{}), "empty must collapse to nil so omitempty drops the key")
	})

	t.Run("same code and message collapses", func(t *testing.T) {
		w := Warning{Code: WarningCodeMkcertCAUntrusted, Message: "CA is not trusted.", Hint: "Run mkcert -install."}
		got := DedupeWarnings([]Warning{w, w, w})
		require.Len(t, got, 1)
		assert.Equal(t, w, got[0], "the first-seen copy, hint and all, is kept")
	})

	t.Run("same code different message is kept", func(t *testing.T) {
		a := Warning{Code: WarningCodeMkcertCAUntrusted, Message: "CA is not trusted."}
		b := Warning{Code: WarningCodeMkcertCAUntrusted, Message: "CA is not trusted for firefox."}
		got := DedupeWarnings([]Warning{a, b})
		assert.Equal(t, []Warning{a, b}, got, "the message is part of the identity, not just the code")
	})

	t.Run("hint is not part of the identity", func(t *testing.T) {
		withHint := Warning{Code: "c", Message: "m", Hint: "do the thing"}
		noHint := Warning{Code: "c", Message: "m"}
		got := DedupeWarnings([]Warning{withHint, noHint})
		require.Len(t, got, 1, "same code+message is one warning to the user regardless of hint")
		assert.Equal(t, withHint, got[0], "the first-seen copy wins, keeping its hint")
	})

	t.Run("first-seen order is preserved", func(t *testing.T) {
		a := Warning{Code: "a", Message: "1"}
		b := Warning{Code: "b", Message: "2"}
		c := Warning{Code: "c", Message: "3"}
		got := DedupeWarnings([]Warning{b, a, b, c, a})
		assert.Equal(t, []Warning{b, a, c}, got)
	})

	t.Run("input is not mutated", func(t *testing.T) {
		in := []Warning{{Code: "a", Message: "1"}, {Code: "a", Message: "1"}, {Code: "b", Message: "2"}}
		_ = DedupeWarnings(in)
		assert.Equal(t, []Warning{{Code: "a", Message: "1"}, {Code: "a", Message: "1"}, {Code: "b", Message: "2"}}, in)
	})
}

// TestWarning_JSONShape pins the wire field names and that an absent Hint is
// omitted (the CLI renders a hintless warning differently).
func TestWarning_JSONShape(t *testing.T) {
	b, err := json.Marshal(Warning{Code: "c", Message: "m"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"code":"c","message":"m"}`, string(b))

	b, err = json.Marshal(Warning{Code: "c", Message: "m", Hint: "h"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"code":"c","message":"m","hint":"h"}`, string(b))
}
