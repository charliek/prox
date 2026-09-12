package domain

import (
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWriter writes a file the way a credential or a user config file has to
// be rewritten: into a temp file in the target's OWN directory, fsynced,
// renamed over the target, with the parent directory fsynced afterwards so the
// rename itself is durable (plan 031 D18/P8). Nothing is ever truncated in
// place, so a crash — or any failure before the rename — leaves the previous
// file byte-identical rather than half-written or empty. A token truncated to
// nothing is the specific failure this exists to make impossible.
//
// It lives in domain because three packages need exactly this sequence and
// none of them may import another: internal/config writes ~/.prox/hubs.yaml,
// internal/proxyd writes ~/.prox/hub.yaml and ~/.prox/hub.token, and
// internal/tui writes ~/.prox/tui/config.toml. Three near-identical copies were
// three chances for the sequence to drift apart.
type AtomicWriter struct {
	// TempPattern is the os.CreateTemp pattern for the temp file, so a temp
	// file that somehow survives names the writer that leaked it. Empty means
	// ".prox-*.tmp".
	TempPattern string

	// The four syscall seams below are nil in production (the real call is
	// used). A test sets one to inject a failure at a specific step and then
	// asserts what the target file looks like afterwards — which is the only
	// way to prove atomicity rather than merely prove a successful write.
	//
	// SyncFn covers EVERY fsync this writer performs — the temp file before
	// the rename, then the target and its directory after it — so a test
	// distinguishes them by call order.
	WriteFn  func(f *os.File, data []byte) (int, error)
	SyncFn   func(f *os.File) error
	CloseFn  func(f *os.File) error
	RenameFn func(oldpath, newpath string) error
}

// AtomicWriteError is what AtomicWriter.WriteFile returns for every failure.
//
// Renamed is the field that matters to callers: it reports whether the rename
// had already succeeded, i.e. whether the NEW bytes are in place and only the
// durability fsync afterwards failed. Before the rename nothing was saved and
// the old file is untouched; after it the save did happen but may not survive a
// power cut. A UI wording a save failure needs exactly that distinction.
type AtomicWriteError struct {
	// Op names the step that failed, e.g. "writing temp file".
	Op string
	// Path is the target file (not the temp file).
	Path string
	// Renamed reports whether the rename already completed.
	Renamed bool
	// Err is the underlying error.
	Err error
}

func (e *AtomicWriteError) Error() string {
	return fmt.Sprintf("%s: %v", e.Op, e.Err)
}

func (e *AtomicWriteError) Unwrap() error {
	return e.Err
}

// WriteFile writes data to path atomically with mode perm. perm is applied to
// the temp file with chmod (not through the create mask), so the target lands
// with exactly the requested bits regardless of umask — which is what makes an
// 0600 credential file actually 0600.
func (w AtomicWriter) WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	pattern := w.TempPattern
	if pattern == "" {
		pattern = ".prox-*.tmp"
	}

	fail := func(op string, err error) error {
		return &AtomicWriteError{Op: op, Path: path, Err: err}
	}

	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fail("creating temp file", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		// Best-effort cleanup of the temp file on every path that did not
		// rename it away. Once renamed, nothing named tmpPath exists.
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fail("setting permissions on temp file", err)
	}
	if _, err := w.write(tmp, data); err != nil {
		_ = tmp.Close()
		return fail("writing temp file", err)
	}
	if err := w.sync(tmp); err != nil {
		_ = tmp.Close()
		return fail("syncing temp file", err)
	}
	if err := w.close(tmp); err != nil {
		return fail("closing temp file", err)
	}
	if err := w.rename(tmpPath, path); err != nil {
		return fail("renaming temp file into place", err)
	}
	renamed = true

	// Past this point the new bytes ARE the file: every remaining failure is a
	// durability failure, not a lost write, and says so via Renamed.
	if err := w.syncPath(path); err != nil {
		return &AtomicWriteError{Op: fmt.Sprintf("syncing %s", path), Path: path, Renamed: true, Err: err}
	}
	if err := w.syncPath(dir); err != nil {
		return &AtomicWriteError{Op: fmt.Sprintf("syncing directory %s", dir), Path: path, Renamed: true, Err: err}
	}
	return nil
}

// syncPath opens path (a file or a directory) and fsyncs it through the SyncFn
// seam.
func (w AtomicWriter) syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return w.sync(f)
}

func (w AtomicWriter) write(f *os.File, data []byte) (int, error) {
	if w.WriteFn != nil {
		return w.WriteFn(f, data)
	}
	return f.Write(data)
}

func (w AtomicWriter) sync(f *os.File) error {
	if w.SyncFn != nil {
		return w.SyncFn(f)
	}
	return f.Sync()
}

func (w AtomicWriter) close(f *os.File) error {
	if w.CloseFn != nil {
		return w.CloseFn(f)
	}
	return f.Close()
}

func (w AtomicWriter) rename(oldpath, newpath string) error {
	if w.RenameFn != nil {
		return w.RenameFn(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}
