package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"github.com/charliek/prox/internal/constants"
)

// DecodedBody is the structured result of loading and content-decoding a
// captured body. It carries decoded (serve-ready) bytes plus the semantics a
// caller needs to render them safely.
type DecodedBody struct {
	// Data is the bytes to serve. For a supported, successful decode these are
	// the decoded bytes; otherwise they are the raw retained bytes.
	Data []byte
	// IsBinary reflects the SERVED bytes (post-decode). It may legitimately
	// diverge from CapturedBody.IsBinary, which describes the raw wire bytes.
	IsBinary bool
	// ContentEncoding is the stored (lowercased/trimmed) content-encoding token,
	// empty for identity/unencoded bodies.
	ContentEncoding string
	// Available is false when the body could not be loaded (e.g. its disk file
	// was evicted). When false, Data is nil and UnavailableReason explains why.
	Available bool
	// UnavailableReason is a short machine-readable reason set only when
	// Available is false (e.g. "evicted").
	UnavailableReason string
}

// LoadCapturedBody returns a captured body's raw retained bytes, reading from
// disk when the body was spilled to a FilePath.
//
//   - A nil body yields (nil, nil).
//   - A body marked Evicted (its data was dropped when the record left the
//     ring's detail window, D9b) yields an os.ErrNotExist-wrapped error, so it
//     travels the same "evicted" path as a body whose spilled file is gone
//     rather than masquerading as an empty body.
//   - An inline body returns a copy of Data (callers must not mutate the record).
//   - A FilePath body MUST resolve within one of allowedDirs; a path that
//     escapes every allowed directory is rejected with an error rather than
//     read. This prevents a socket-supplied path from exfiltrating arbitrary
//     files through the project API.
//   - os.ReadFile errors (missing/evicted file) propagate to the caller.
func LoadCapturedBody(body *CapturedBody, allowedDirs []string) ([]byte, error) {
	if body == nil {
		return nil, nil
	}

	if body.Evicted {
		return nil, fmt.Errorf("captured body evicted by request retention: %w", os.ErrNotExist)
	}

	if body.Data != nil {
		result := make([]byte, len(body.Data))
		copy(result, body.Data)
		return result, nil
	}

	if body.FilePath == "" {
		return nil, nil
	}

	resolved, err := resolveWithinAllowedDirs(body.FilePath, allowedDirs)
	if err != nil {
		return nil, err
	}

	return os.ReadFile(resolved)
}

// resolveWithinAllowedDirs makes path absolute, resolves every symlink in it
// (the file itself and its directory components), and verifies the RESOLVED
// path sits inside one of allowedDirs — each of which is symlink-resolved too,
// so a capture dir reached via a symlinked parent (e.g. macOS /var → /private/
// var temp dirs) still matches. The symlink resolution closes the bypass where
// a file inside an allowed dir is a symlink pointing outside it; the remaining
// check-then-read race is accepted under the same-user trust model. A missing
// file surfaces as EvalSymlinks' not-exist error (the evicted case upstream).
func resolveWithinAllowedDirs(path string, allowedDirs []string) (string, error) {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolving captured body path %q: %w", path, err)
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", err
	}

	for _, dir := range allowedDirs {
		if dir == "" {
			continue
		}
		absDir, err := filepath.Abs(filepath.Clean(dir))
		if err != nil {
			continue
		}
		resolvedDir, err := filepath.EvalSymlinks(absDir)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(resolvedDir, resolvedPath)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if filepath.IsAbs(rel) {
			continue
		}
		return resolvedPath, nil
	}

	return "", fmt.Errorf("captured body path %q is outside the allowed capture directories", path)
}

// CaptureAllowedDirs builds the FilePath allowlist passed to LoadCapturedBody:
// primaryDir (the caller's own capture dir, skipped when empty) plus the shared
// daemon capture dir under the user's home (skipped when the home dir cannot be
// resolved). Centralizing keeps the "the daemon capture dir is always allowed"
// policy in one place rather than duplicated across the api and tui callers.
func CaptureAllowedDirs(primaryDir string) []string {
	var dirs []string
	if primaryDir != "" {
		dirs = append(dirs, primaryDir)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, constants.DaemonCaptureDir(home))
	}
	return dirs
}

// DecodeCapturedBody content-decodes raw (per body.ContentEncoding) and returns
// serve-ready bytes with post-decode binary semantics.
//
// Supported encodings are gzip/x-gzip, deflate (zlib-wrapped per RFC 9110,
// with a fallback to raw deflate for servers that send it unwrapped), zstd,
// and br (case-insensitive, surrounding whitespace tolerated); identity/empty
// means no decode. Any other token (chained values like "gzip, br"), a
// truncated body, a decode failure, or a decoded size exceeding the cap all
// fall back to serving the raw bytes with IsBinary=true so a JSON string
// conversion cannot mangle them.
func DecodeCapturedBody(body *CapturedBody, raw []byte) DecodedBody {
	token := strings.ToLower(strings.TrimSpace(body.ContentEncoding))

	switch token {
	case "", "identity":
		return DecodedBody{
			Data:      raw,
			IsBinary:  body.IsBinary,
			Available: true,
		}
	case "gzip", "x-gzip":
		return decodeWithFallback(body, raw, token, gunzipLimited)
	case "deflate":
		return decodeWithFallback(body, raw, token, inflateLimited)
	case "zstd":
		return decodeWithFallback(body, raw, token, zstdLimited)
	case "br":
		return decodeWithFallback(body, raw, token, brotliLimited)
	default:
		// Unsupported encoding: serve raw, flagged binary.
		return rawEncoded(raw, token)
	}
}

// decodeWithFallback runs decodeFn (one of the bounded per-codec decoders
// below) against raw and returns serve-ready decoded bytes, or the raw
// fallback when the body was truncated, the decode failed, or the decoded
// size exceeded the cap. Shared by every content-encoding case so the
// truncated-short-circuit and cap-exceeded-fallback behavior stay identical
// across codecs.
func decodeWithFallback(body *CapturedBody, raw []byte, token string, decodeFn func([]byte, int64) ([]byte, bool)) DecodedBody {
	// A truncated stream is broken; do not attempt to decode it.
	if body.Truncated {
		return rawEncoded(raw, token)
	}
	decoded, ok := decodeFn(raw, constants.MaxDecodedBodySize)
	if !ok {
		return rawEncoded(raw, token)
	}
	return DecodedBody{
		Data:            decoded,
		IsBinary:        isBinaryContent(decoded, body.ContentType),
		ContentEncoding: token,
		Available:       true,
	}
}

// rawEncoded returns the raw bytes served as opaque binary, preserving the
// stored encoding token for reporting.
func rawEncoded(raw []byte, token string) DecodedBody {
	return DecodedBody{
		Data:            raw,
		IsBinary:        true,
		ContentEncoding: token,
		Available:       true,
	}
}

// decodeLimited drains r, capping output at limit bytes. It returns (nil,
// false) on a read/stream error or when the decoded size would exceed the
// cap (zip-bomb guard); memory is bounded to limit+1 bytes regardless of
// codec. Every bounded decoder below (gzip, deflate, zstd, br) funnels
// through this so the cap-exceeded and stream-error semantics stay identical
// across encodings.
func decodeLimited(r io.Reader, limit int64) ([]byte, bool) {
	// Read one byte past the cap so an over-cap payload is detectable.
	decoded, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false
	}
	if int64(len(decoded)) > limit {
		return nil, false
	}
	return decoded, true
}

// gunzipLimited gzip-decodes raw, capping output at limit bytes. It returns
// (nil, false) on a header/stream error or when the decoded size would exceed
// the cap.
func gunzipLimited(raw []byte, limit int64) ([]byte, bool) {
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	defer gr.Close()
	return decodeLimited(gr, limit)
}

// inflateLimited deflate-decodes raw, capping output at limit bytes.
//
// RFC 9110 defines the "deflate" content-coding as a zlib-wrapped deflate
// stream (RFC 1950 header around an RFC 1951 payload), so zlib is tried
// first. Some servers instead send raw deflate with no zlib header; that
// case is detected specifically as zlib.ErrHeader from zlib.NewReader (the
// two-byte CMF/FLG header doesn't look like zlib) and retried against
// compress/flate. A zlib stream that opens fine but fails later — a
// mid-stream corruption error — is a genuinely broken stream and must NOT be
// silently retried as raw deflate; it falls straight through to the raw
// fallback like any other decode failure.
func inflateLimited(raw []byte, limit int64) ([]byte, bool) {
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		if !errors.Is(err, zlib.ErrHeader) {
			return nil, false
		}
		fr := flate.NewReader(bytes.NewReader(raw))
		defer fr.Close()
		return decodeLimited(fr, limit)
	}
	defer zr.Close()
	return decodeLimited(zr, limit)
}

// zstdLimited zstd-decodes raw, capping output at limit bytes. It returns
// (nil, false) on a header/stream error or when the decoded size would
// exceed the cap. WithDecoderMaxMemory bounds the decoder's own window/
// in-memory allocation to limit bytes as a defense-in-depth measure on top of
// decodeLimited's output cap, so a hostile stream can't force a large
// allocation before the output-size check ever runs.
func zstdLimited(raw []byte, limit int64) ([]byte, bool) {
	// WithDecoderConcurrency(1) keeps the decode synchronous: the default
	// spawns per-decoder goroutines, which an adversarial stream could
	// multiply for CPU burn before the output cap bites. One captured body
	// decodes at a time here; concurrency buys nothing.
	zr, err := zstd.NewReader(bytes.NewReader(raw),
		zstd.WithDecoderMaxMemory(uint64(limit)), zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, false
	}
	defer zr.Close()
	return decodeLimited(zr, limit)
}

// brotliLimited brotli-decodes raw, capping output at limit bytes. It
// returns (nil, false) on a stream error or when the decoded size would
// exceed the cap.
func brotliLimited(raw []byte, limit int64) ([]byte, bool) {
	br := brotli.NewReader(bytes.NewReader(raw))
	return decodeLimited(br, limit)
}

// LoadDecodedBody composes LoadCapturedBody + DecodeCapturedBody.
//
// A nil body yields an unavailable result. A body whose data is simply gone —
// a missing capture file (the record is still valid, but its FilePath body was
// evicted/removed) or a body the ring marked Evicted on leaving the detail
// window (D9b) — is treated as a benign condition (D7): the result is marked
// unavailable with reason "evicted" and a nil error, so the caller returns HTTP
// 200 with no data rather than failing the request. Both cases are detected as
// errors.Is(err, fs.ErrNotExist) — not os.IsNotExist, which does not unwrap
// %w-wrapped sentinels. Any other load failure (e.g. an out-of-allowlist path
// or an I/O error) is marked unavailable with reason "unavailable" and returned
// together with the underlying error so callers can log it.
func LoadDecodedBody(body *CapturedBody, allowedDirs []string) (DecodedBody, error) {
	if body == nil {
		return DecodedBody{Available: false}, nil
	}

	raw, err := LoadCapturedBody(body, allowedDirs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DecodedBody{Available: false, UnavailableReason: "evicted"}, nil
		}
		return DecodedBody{Available: false, UnavailableReason: "unavailable"}, err
	}

	return DecodeCapturedBody(body, raw), nil
}
