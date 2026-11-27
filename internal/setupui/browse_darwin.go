//go:build darwin

package setupui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// isExcludedDarwinVolume reports whether a /Volumes entry is a hidden entry or
// macOS internal system/snapshot mount (such as com.apple.TimeMachine.localsnapshots)
// that should not appear as a user photo volume in the setup UI quick-access bar.
func isExcludedDarwinVolume(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), "com.apple.")
}

func platformBrowseRoots() []BrowseEntry {
	return darwinVolumesFromDir("/Volumes")
}

func darwinVolumesFromDir(volumesDir string) []BrowseEntry {
	entries, err := os.ReadDir(volumesDir)
	if err != nil {
		return nil
	}
	var out []BrowseEntry
	for _, e := range entries {
		name := e.Name()
		if isExcludedDarwinVolume(name) {
			continue
		}
		full := filepath.Join(volumesDir, name)
		if info, err := os.Stat(full); err == nil && info.IsDir() {
			out = append(out, BrowseEntry{Name: name, Path: full})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}
