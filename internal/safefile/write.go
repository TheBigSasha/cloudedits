package safefile

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with data so a reader sees either the old or
// the new contents, never a torn file. The temporary file has a unique name
// beside path, so concurrent writers of the same path cannot interleave
// bytes, and it is removed when any step fails. It does not fsync: callers
// write app-data (config, status, service units, caches) that the next run
// can rebuild, not the user's sidecars, which take the durable sidecar path.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
