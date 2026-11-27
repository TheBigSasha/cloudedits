//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// checkSidecarRootWritable reports whether the current process can create or
// write sidecar files under sidecarRoot without creating any directory or
// touching any photo folder mtime. It walks up to the nearest existing
// directory and asks the kernel via access(W_OK), which honors UID/GID,
// supplementary groups, ACLs, and read-only mount flags.
func checkSidecarRootWritable(sidecarRoot string) error {
	p := filepath.Clean(sidecarRoot)
	for {
		info, err := os.Stat(p)
		if err == nil {
			if !info.IsDir() {
				return errors.New("not a directory")
			}
			if aerr := unix.Access(p, unix.W_OK); aerr != nil {
				return aerr
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return err
		}
		p = parent
	}
}
