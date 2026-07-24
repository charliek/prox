package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"os"
	"path/filepath"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
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

// zlibBytes returns the zlib-wrapped deflate form of data (RFC 1950 header
// around an RFC 1951 payload) — the "deflate" content-coding per RFC 9110.
func zlibBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// rawDeflateBytes returns a bare RFC 1951 deflate stream with no zlib
// header — what some servers send despite advertising "deflate".
func rawDeflateBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	require.NoError(t, err)
	_, err = fw.Write(data)
	require.NoError(t, err)
	require.NoError(t, fw.Close())
	return buf.Bytes()
}

// zstdBytes returns the zstd-compressed form of data.
func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	require.NoError(t, err)
	_, err = zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// brotliBytes returns the brotli-compressed form of data.
func brotliBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	bw := brotli.NewWriter(&buf)
	_, err := bw.Write(data)
	require.NoError(t, err)
	require.NoError(t, bw.Close())
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
		// "compress" (legacy LZW) is a real-but-unimplemented token; "gzip, br"
		// is a chained encoding, which stays unsupported regardless of whether
		// the individual codecs it names are each supported.
		for _, enc := range []string{"compress", "gzip, br"} {
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

// TestDecodeCapturedBody_D10Codecs table-drives the same behaviors already
// proven for gzip above (roundtrip, corrupt-stream fallback, truncated
// short-circuit, cap-exceeded fallback) across the D10 codecs: deflate
// (both zlib-wrapped and raw), zstd, and br.
func TestDecodeCapturedBody_D10Codecs(t *testing.T) {
	jsonPayload := []byte(`{"hello":"world","n":42}`)

	type codecCase struct {
		name   string
		token  string
		encode func(t *testing.T, data []byte) []byte
		// corruptIndex picks which byte of the encoded stream to flip to
		// produce a genuinely corrupt (decode-failing) stream. Defaults to
		// len(raw)-3 (same recipe as the pre-existing gzip test) when nil;
		// brotli has no trailing checksum, so flipping near the end can land
		// on padding bits the decoder tolerates — corrupting byte 0 (the
		// stream header) reliably fails instead.
		corruptIndex func(raw []byte) int
	}

	nearEnd := func(raw []byte) int { return len(raw) - 3 }

	cases := []codecCase{
		{"deflate (zlib-wrapped, RFC 9110)", "deflate", zlibBytes, nearEnd},
		{"deflate (raw, no zlib header)", "deflate", rawDeflateBytes, nearEnd},
		{"zstd", "zstd", zstdBytes, nearEnd},
		{"br", "br", brotliBytes, func(raw []byte) int { return 0 }},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/roundtrip decodes", func(t *testing.T) {
			raw := tc.encode(t, jsonPayload)
			body := &CapturedBody{ContentEncoding: tc.token, ContentType: "application/json", IsBinary: true}
			got := DecodeCapturedBody(body, raw)
			assert.True(t, got.Available)
			assert.False(t, got.IsBinary) // decoded JSON is text
			assert.Equal(t, jsonPayload, got.Data)
			assert.Equal(t, tc.token, got.ContentEncoding)
		})

		t.Run(tc.name+"/corrupt stream falls back to raw binary", func(t *testing.T) {
			raw := tc.encode(t, jsonPayload)
			raw[tc.corruptIndex(raw)] ^= 0xff
			body := &CapturedBody{ContentEncoding: tc.token, ContentType: "application/json"}
			got := DecodeCapturedBody(body, raw)
			assert.True(t, got.Available)
			assert.True(t, got.IsBinary)
			assert.Equal(t, raw, got.Data)
			assert.Equal(t, tc.token, got.ContentEncoding)
		})

		t.Run(tc.name+"/truncated body short-circuits to raw", func(t *testing.T) {
			raw := tc.encode(t, jsonPayload)
			body := &CapturedBody{ContentEncoding: tc.token, ContentType: "application/json", Truncated: true}
			got := DecodeCapturedBody(body, raw)
			assert.True(t, got.Available)
			assert.True(t, got.IsBinary)
			assert.Equal(t, raw, got.Data)
			assert.Equal(t, tc.token, got.ContentEncoding)
		})

		t.Run(tc.name+"/decode cap exceeded falls back to raw binary", func(t *testing.T) {
			// Highly compressible payload larger than the 10MB decode cap.
			bomb := bytes.Repeat([]byte{'a'}, 11*1024*1024)
			raw := tc.encode(t, bomb)
			require.Less(t, len(raw), len(bomb)) // compressed is much smaller
			body := &CapturedBody{ContentEncoding: tc.token, ContentType: "text/plain"}
			got := DecodeCapturedBody(body, raw)
			assert.True(t, got.Available)
			assert.True(t, got.IsBinary)
			assert.Equal(t, raw, got.Data) // served raw, not the expanded bomb
		})
	}
}

// TestInflateLimited unit-tests the deflate zlib-vs-raw dispatch directly
// (same package, so the unexported helper is reachable), pinning the D10
// requirement that a mid-stream zlib corruption must NOT be silently
// retried as raw deflate — only a zlib *header* error triggers the retry.
func TestInflateLimited(t *testing.T) {
	jsonPayload := []byte(`{"hello":"world"}`)
	const limit = 10 * 1024 * 1024

	t.Run("zlib-wrapped stream decodes directly", func(t *testing.T) {
		raw := zlibBytes(t, jsonPayload)
		decoded, ok := inflateLimited(raw, limit)
		require.True(t, ok)
		assert.Equal(t, jsonPayload, decoded)
	})

	t.Run("raw deflate falls back from zlib header error and decodes", func(t *testing.T) {
		raw := rawDeflateBytes(t, jsonPayload)
		decoded, ok := inflateLimited(raw, limit)
		require.True(t, ok)
		assert.Equal(t, jsonPayload, decoded)
	})

	t.Run("mid-stream zlib corruption is not retried as raw deflate", func(t *testing.T) {
		raw := zlibBytes(t, jsonPayload)
		raw[len(raw)-3] ^= 0xff // corrupt payload bytes; the 2-byte zlib header is untouched
		_, ok := inflateLimited(raw, limit)
		assert.False(t, ok)
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

	t.Run("allowlist violation surfaces error with unavailable reason", func(t *testing.T) {
		allowed := t.TempDir()
		outside := t.TempDir()
		fp := filepath.Join(outside, "secret.bin")
		require.NoError(t, os.WriteFile(fp, []byte("secret"), 0600))
		body := &CapturedBody{FilePath: fp}
		got, err := LoadDecodedBody(body, []string{allowed})
		require.Error(t, err) // out-of-allowlist path is not benign
		assert.False(t, got.Available)
		assert.Equal(t, "unavailable", got.UnavailableReason)
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
