//go:build darwin

package setupui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func platformBrowseRoots() []BrowseEntry {
	entries, err := os.ReadDir("/Volumes")
	if err != nil {
		return nil
	}
	var out []BrowseEntry
	for _, e := range entries {
		name := e.Name()
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join("/Volumes", name)
		if info, err := os.Stat(full); err == nil && info.IsDir() {
			out = append(out, BrowseEntry{Name: name, Path: full})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}
