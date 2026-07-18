package tui

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charliek/prox/internal/proxy"
)

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(data)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

// TestConvertRequestRecordToDetail_FilePathBacked pins that a FilePath-backed
// (disk-spilled) body is loaded — previously the TUI conversion read Data
// directly and dropped disk bodies.
func TestConvertRequestRecordToDetail_FilePathBacked(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"disk":"backed"}`)
	fp := filepath.Join(dir, "res.bin")
	require.NoError(t, os.WriteFile(fp, payload, 0600))

	rec := proxy.RequestRecord{
		ID:        "disk001",
		Timestamp: time.Now(),
		Method:    "GET",
		URL:       "/data",
		Details: &proxy.RequestDetails{
			ResponseBody: &proxy.CapturedBody{
				Size:         int64(len(payload)),
				CapturedSize: int64(len(payload)),
				ContentType:  "application/json",
				FilePath:     fp,
			},
		},
	}

	detail := convertRequestRecordToDetailWithDirs(rec, []string{dir})
	require.NotNil(t, detail.ResponseBody)
	assert.False(t, detail.ResponseBody.Unavailable)
	assert.False(t, detail.ResponseBody.IsBinary)
	assert.Equal(t, string(payload), detail.ResponseBody.Data)
}

// TestConvertCapturedBodyToBodyData_InvalidUTF8NotStringified pins the TUI's
// defense-in-depth: bytes that are not valid UTF-8 are never string-converted
// for rendering, even when the stored record claims IsBinary=false (e.g. a
// socket-supplied flag or a disk file mutated after classification).
func TestConvertCapturedBodyToBodyData_InvalidUTF8NotStringified(t *testing.T) {
	body := &proxy.CapturedBody{
		Size:         4,
		CapturedSize: 4,
		ContentType:  "text/plain",
		IsBinary:     false, // lies: data is not valid UTF-8
		Data:         []byte{'h', 'i', 0xFF, 0xFE},
	}

	bd := convertCapturedBodyToBodyData(body, nil)
	require.NotNil(t, bd)
	assert.True(t, bd.IsBinary, "invalid UTF-8 must be reclassified binary")
	assert.Empty(t, bd.Data, "invalid UTF-8 must not be string-converted")
}

func TestConvertRequestRecordToDetail_GzipFilePath(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"gzip":"on disk"}`)
	fp := filepath.Join(dir, "res.bin")
	require.NoError(t, os.WriteFile(fp, gzipBytes(t, payload), 0600))

	rec := proxy.RequestRecord{
		ID: "gz1",
		Details: &proxy.RequestDetails{
			ResponseBody: &proxy.CapturedBody{
				ContentType:     "application/json",
				ContentEncoding: "gzip",
				IsBinary:        true,
				FilePath:        fp,
			},
		},
	}

	detail := convertRequestRecordToDetailWithDirs(rec, []string{dir})
	require.NotNil(t, detail.ResponseBody)
	assert.False(t, detail.ResponseBody.IsBinary)
	assert.Equal(t, string(payload), detail.ResponseBody.Data)
}

func TestConvertRequestRecordToDetail_EvictedFilePath(t *testing.T) {
	dir := t.TempDir()
	rec := proxy.RequestRecord{
		ID: "ev1",
		Details: &proxy.RequestDetails{
			ResponseBody: &proxy.CapturedBody{
				Size:        10,
				ContentType: "application/json",
				FilePath:    filepath.Join(dir, "gone.bin"),
			},
		},
	}

	detail := convertRequestRecordToDetailWithDirs(rec, []string{dir})
	require.NotNil(t, detail.ResponseBody)
	assert.True(t, detail.ResponseBody.Unavailable)
	assert.Equal(t, "evicted", detail.ResponseBody.UnavailableReason)
	assert.Empty(t, detail.ResponseBody.Data)
}
