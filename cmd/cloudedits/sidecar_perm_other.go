//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
)

func checkSidecarRootWritable(sidecarRoot string) error {
	p := filepath.Clean(sidecarRoot)
	for {
		info, err := os.Stat(p)
		if err == nil {
			if !info.IsDir() {
				return errors.New("not a directory")
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
