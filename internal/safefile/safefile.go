// Package safefile reads files that sit in folders other software (or other
// people) can write to, without trusting what a directory entry points at.
package safefile

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNotRegular reports a directory entry that is not a plain file: a symbolic
// link, device, FIFO, socket or directory.
var ErrNotRegular = errors.New("not a regular file")

// ErrTooLarge reports a file larger than the caller's limit.
var ErrTooLarge = errors.New("file exceeds size limit")

// ReadRegular returns the contents of path only if it names a regular file of
// at most limit bytes. It never follows a symbolic link in the final path
// element, so a link planted where a sidecar is expected cannot make the
// server disclose another file, block on a FIFO, or read a device without
// end; and it never reads more than limit bytes, so an oversized file cannot
// exhaust memory. A missing file reports an error matching os.ErrNotExist.
func ReadRegular(path string, limit int64) ([]byte, os.FileInfo, error) {
	return readRegular(path, limit, os.Lstat, os.Open)
}

// ReadRegularInRoot returns the contents of rel beneath root only if it names
// a regular file of at most limit bytes without escaping root via any
// intermediate or final symbolic link.
func ReadRegularInRoot(root *os.Root, rel string, limit int64) ([]byte, os.FileInfo, error) {
	return readRegular(rel, limit, root.Lstat, root.Open)
}

func readRegular(name string, limit int64, lstat func(string) (os.FileInfo, error), open func(string) (*os.File, error)) ([]byte, os.FileInfo, error) {
	before, err := lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, name)
	}
	if before.Size() > limit {
		return nil, nil, fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrTooLarge, name, before.Size(), limit)
	}

	f, err := open(name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	// The entry may have been swapped between Lstat and Open: require the
	// opened file to be the one that was inspected.
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, name)
	}

	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("%w: %s (limit %d)", ErrTooLarge, name, limit)
	}
	return data, info, nil
}
