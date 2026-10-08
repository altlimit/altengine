package common

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with data so that a reader — or the next start after a crash or
// a full disk — sees either the old file or the new one, never a truncated mix. It writes a temp
// file in the same directory (a rename is only atomic within one filesystem), fsyncs it, and
// renames it over path. On any error the previous file is left exactly as it was.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	// Make the rename itself durable. Not every platform can open a directory to sync it, and
	// the file is already in place, so this is best effort.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
