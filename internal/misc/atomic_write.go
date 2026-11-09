package misc

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// WriteFileAtomic replaces path with data so that a reader, or the next start after a crash,
// sees either the old file or the complete new one, never a truncated mix. It writes a
// temporary file next to path, syncs it and renames it over path.
//
// Credential files depend on this: an OAuth refresh rotates the refresh token, and a
// truncate-then-write that fails halfway (for example on a full disk) would leave the account
// without any valid token.
//
// A symlink is followed and its target replaced, and an existing file keeps its permission
// bits (perm applies to new files). When the target cannot be replaced by a rename (a file
// bind-mounted on its own, EBUSY/EXDEV), it falls back to writing in place.
//
// The temporary name starts with "." and ends in ".tmp", so watchers that only pick up
// "*.json" files ignore it.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	if resolved, errEval := filepath.EvalSymlinks(path); errEval == nil {
		path = resolved
	}
	if st, errStat := os.Stat(path); errStat == nil && st.Mode().IsRegular() {
		perm = st.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		if errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EXDEV) {
			_ = os.Remove(tmpName)
			err = os.WriteFile(path, data, perm)
		}
		return err
	}
	// Persist the rename itself; a failure here does not undo the replacement.
	if d, errOpen := os.Open(dir); errOpen == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
