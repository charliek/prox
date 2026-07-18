package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
// Supported encodings are gzip and x-gzip (case-insensitive, surrounding
// whitespace tolerated); identity/empty means no decode. Any other token
// (deflate, br, zstd, chained values like "gzip, br"), a truncated body, a
// decode failure, or a decoded size exceeding the cap all fall back to serving
// the raw bytes with IsBinary=true so a JSON string conversion cannot mangle
// them.
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
		// A truncated stream is broken; do not attempt to decode it.
		if body.Truncated {
			return rawEncoded(raw, token)
		}
		decoded, ok := gunzipLimited(raw, constants.MaxDecodedBodySize)
		if !ok {
			return rawEncoded(raw, token)
		}
		return DecodedBody{
			Data:            decoded,
			IsBinary:        isBinaryContent(decoded, body.ContentType),
			ContentEncoding: token,
			Available:       true,
		}
	default:
		// Unsupported encoding: serve raw, flagged binary.
		return rawEncoded(raw, token)
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

// gunzipLimited gzip-decodes raw, capping output at limit bytes. It returns
// (nil, false) on a header/stream error or when the decoded size would exceed
// the cap (zip-bomb guard); memory is bounded to limit+1 bytes.
func gunzipLimited(raw []byte, limit int64) ([]byte, bool) {
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	defer gr.Close()

	// Read one byte past the cap so an over-cap payload is detectable.
	decoded, err := io.ReadAll(io.LimitReader(gr, limit+1))
	if err != nil {
		return nil, false
	}
	if int64(len(decoded)) > limit {
		return nil, false
	}
	return decoded, true
}

// LoadDecodedBody composes LoadCapturedBody + DecodeCapturedBody.
//
// A nil body yields an unavailable result. A missing capture file (the record
// is still valid, but its FilePath body was evicted/removed) is treated as a
// benign condition (D7): the result is marked unavailable with reason "evicted"
// and a nil error, so the caller returns HTTP 200 with no data rather than
// failing the request. Any other load failure (e.g. an out-of-allowlist path or
// an I/O error) is marked unavailable with reason "unavailable" and returned
// together with the underlying error so callers can log it.
func LoadDecodedBody(body *CapturedBody, allowedDirs []string) (DecodedBody, error) {
	if body == nil {
		return DecodedBody{Available: false}, nil
	}

	raw, err := LoadCapturedBody(body, allowedDirs)
	if err != nil {
		if os.IsNotExist(err) {
			return DecodedBody{Available: false, UnavailableReason: "evicted"}, nil
		}
		return DecodedBody{Available: false, UnavailableReason: "unavailable"}, err
	}

	return DecodeCapturedBody(body, raw), nil
}
