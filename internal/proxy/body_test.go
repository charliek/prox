package proxy

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gzipBytes returns the gzip-compressed form of data.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(data)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func TestLoadCapturedBody(t *testing.T) {
	t.Run("nil body returns nil", func(t *testing.T) {
		data, err := LoadCapturedBody(nil, nil)
		require.NoError(t, err)
		assert.Nil(t, data)
	})

	t.Run("inline data returns a copy", func(t *testing.T) {
		original := []byte("hello")
		body := &CapturedBody{Data: original}
		got, err := LoadCapturedBody(body, nil)
		require.NoError(t, err)
		assert.Equal(t, original, got)
		got[0] = 'X'
		assert.Equal(t, byte('h'), original[0]) // original untouched
	})

	t.Run("file inside an allowed dir loads", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "body.bin")
		require.NoError(t, os.WriteFile(fp, []byte("disk content"), 0600))

		body := &CapturedBody{FilePath: fp}
		got, err := LoadCapturedBody(body, []string{dir})
		require.NoError(t, err)
		assert.Equal(t, "disk content", string(got))
	})

	t.Run("file outside all allowed dirs is rejected", func(t *testing.T) {
		allowed := t.TempDir()
		other := t.TempDir()
		fp := filepath.Join(other, "body.bin")
		require.NoError(t, os.WriteFile(fp, []byte("secret"), 0600))

		body := &CapturedBody{FilePath: fp}
		_, err := LoadCapturedBody(body, []string{allowed})
		require.Error(t, err)
	})

	t.Run("dot-dot escape is rejected", func(t *testing.T) {
		allowed := t.TempDir()
		// A path that lexically climbs out of the allowed dir.
		escape := filepath.Join(allowed, "..", "outside.bin")
		body := &CapturedBody{FilePath: escape}
		_, err := LoadCapturedBody(body, []string{allowed})
		require.Error(t, err)
	})

	t.Run("missing file inside an allowed dir propagates os error", func(t *testing.T) {
		dir := t.TempDir()
		body := &CapturedBody{FilePath: filepath.Join(dir, "gone.bin")}
		_, err := LoadCapturedBody(body, []string{dir})
		require.Error(t, err)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("symlink inside an allowed dir pointing outside is rejected", func(t *testing.T) {
		allowed := t.TempDir()
		outside := t.TempDir()
		secret := filepath.Join(outside, "secret.bin")
		require.NoError(t, os.WriteFile(secret, []byte("secret"), 0600))

		link := filepath.Join(allowed, "body.bin")
		require.NoError(t, os.Symlink(secret, link))

		body := &CapturedBody{FilePath: link}
		_, err := LoadCapturedBody(body, []string{allowed})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "outside the allowed capture directories")
	})

	t.Run("allowed dir reached via symlinked parent still loads", func(t *testing.T) {
		realDir := t.TempDir()
		linkParent := t.TempDir()
		linkedDir := filepath.Join(linkParent, "capture-link")
		require.NoError(t, os.Symlink(realDir, linkedDir))

		fp := filepath.Join(linkedDir, "body.bin")
		require.NoError(t, os.WriteFile(fp, []byte("via symlinked dir"), 0600))

		body := &CapturedBody{FilePath: fp}
		got, err := LoadCapturedBody(body, []string{linkedDir})
		require.NoError(t, err)
		assert.Equal(t, "via symlinked dir", string(got))
	})
}

func TestDecodeCapturedBody(t *testing.T) {
	jsonPayload := []byte(`{"hello":"world","n":42}`)

	t.Run("identity passes through with stored binary flag", func(t *testing.T) {
		body := &CapturedBody{ContentEncoding: "identity", IsBinary: false, ContentType: "application/json"}
		got := DecodeCapturedBody(body, jsonPayload)
		assert.True(t, got.Available)
		assert.False(t, got.IsBinary)
		assert.Equal(t, jsonPayload, got.Data)
	})

	t.Run("empty encoding passes through", func(t *testing.T) {
		body := &CapturedBody{IsBinary: true}
		got := DecodeCapturedBody(body, []byte{0x00, 0x01})
		assert.True(t, got.Available)
		assert.True(t, got.IsBinary)
	})

	t.Run("gzip decodes to readable text", func(t *testing.T) {
		raw := gzipBytes(t, jsonPayload)
		body := &CapturedBody{
			ContentEncoding: "gzip",
			ContentType:     "application/json",
			IsBinary:        true, // raw gzip bytes are binary
		}
		got := DecodeCapturedBody(body, raw)
		assert.True(t, got.Available)
		assert.False(t, got.IsBinary) // decoded JSON is text
		assert.Equal(t, jsonPayload, got.Data)
		assert.Equal(t, "gzip", got.ContentEncoding)
	})

	t.Run("x-gzip alias decodes", func(t *testing.T) {
		raw := gzipBytes(t, jsonPayload)
		body := &CapturedBody{ContentEncoding: "x-gzip", ContentType: "application/json"}
		got := DecodeCapturedBody(body, raw)
		assert.Equal(t, jsonPayload, got.Data)
		assert.False(t, got.IsBinary)
		assert.Equal(t, "x-gzip", got.ContentEncoding)
	})

	t.Run("uppercase GZIP with whitespace decodes", func(t *testing.T) {
		raw := gzipBytes(t, jsonPayload)
		body := &CapturedBody{ContentEncoding: "  GZIP  ", ContentType: "application/json"}
		got := DecodeCapturedBody(body, raw)
		assert.Equal(t, jsonPayload, got.Data)
		assert.False(t, got.IsBinary)
	})

	t.Run("corrupt gzip stream falls back to raw binary", func(t *testing.T) {
		raw := gzipBytes(t, jsonPayload)
		raw[len(raw)-3] ^= 0xff // corrupt near the end
		body := &CapturedBody{ContentEncoding: "gzip", ContentType: "application/json"}
		got := DecodeCapturedBody(body, raw)
		assert.True(t, got.Available)
		assert.True(t, got.IsBinary)
		assert.Equal(t, raw, got.Data)
		assert.Equal(t, "gzip", got.ContentEncoding)
	})

	t.Run("truncated gzip is not decoded", func(t *testing.T) {
		raw := gzipBytes(t, jsonPayload)
		body := &CapturedBody{ContentEncoding: "gzip", ContentType: "application/json", Truncated: true}
		got := DecodeCapturedBody(body, raw)
		assert.True(t, got.IsBinary)
		assert.Equal(t, raw, got.Data)
	})

	t.Run("unsupported encodings fall back to raw binary", func(t *testing.T) {
		for _, enc := range []string{"br", "deflate", "zstd", "gzip, br"} {
			body := &CapturedBody{ContentEncoding: enc, ContentType: "application/json"}
			got := DecodeCapturedBody(body, jsonPayload)
			assert.True(t, got.Available, enc)
			assert.True(t, got.IsBinary, enc)
			assert.Equal(t, jsonPayload, got.Data, enc)
			assert.Equal(t, enc, got.ContentEncoding, enc)
		}
	})

	t.Run("decode cap exceeded (zip bomb) falls back to raw binary", func(t *testing.T) {
		// Highly compressible payload larger than the 10MB decode cap.
		bomb := bytes.Repeat([]byte{'a'}, 11*1024*1024)
		raw := gzipBytes(t, bomb)
		require.Less(t, len(raw), len(bomb)) // compressed is much smaller
		body := &CapturedBody{ContentEncoding: "gzip", ContentType: "text/plain"}
		got := DecodeCapturedBody(body, raw)
		assert.True(t, got.IsBinary)
		assert.Equal(t, raw, got.Data) // served raw, not the expanded bomb
	})
}

func TestLoadDecodedBody(t *testing.T) {
	t.Run("nil body is unavailable without reason", func(t *testing.T) {
		got, err := LoadDecodedBody(nil, nil)
		require.NoError(t, err)
		assert.False(t, got.Available)
	})

	t.Run("evicted file yields unavailable with nil error", func(t *testing.T) {
		dir := t.TempDir()
		body := &CapturedBody{FilePath: filepath.Join(dir, "evicted.bin")}
		got, err := LoadDecodedBody(body, []string{dir})
		require.NoError(t, err) // record still valid (D7)
		assert.False(t, got.Available)
		assert.Equal(t, "evicted", got.UnavailableReason)
		assert.Nil(t, got.Data)
	})

	t.Run("disk-backed gzip body loads and decodes", func(t *testing.T) {
		dir := t.TempDir()
		payload := []byte(`{"disk":"gzip"}`)
		fp := filepath.Join(dir, "res.bin")
		require.NoError(t, os.WriteFile(fp, gzipBytes(t, payload), 0600))

		body := &CapturedBody{
			FilePath:        fp,
			ContentEncoding: "gzip",
			ContentType:     "application/json",
			IsBinary:        true,
		}
		got, err := LoadDecodedBody(body, []string{dir})
		require.NoError(t, err)
		assert.True(t, got.Available)
		assert.False(t, got.IsBinary)
		assert.Equal(t, payload, got.Data)
	})
}
