package setupui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxBrowseEntries = 1000

// BrowseEntry represents one directory in the read-only folder browser.
type BrowseEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// BrowseResponse is returned by GET /api/browse-folders.
type BrowseResponse struct {
	Path    string        `json:"path"`
	Parent  string        `json:"parent"`
	Roots   []BrowseEntry `json:"roots"`
	Entries []BrowseEntry `json:"entries"`
}

func quickAccessRoots() []BrowseEntry {
	var roots []BrowseEntry
	seen := map[string]bool{}
	add := func(name, p string) {
		if p == "" {
			return
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return
		}
		key := strings.ToLower(abs)
		if seen[key] {
			return
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return
		}
		seen[key] = true
		roots = append(roots, BrowseEntry{Name: name, Path: abs})
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add("Pictures", filepath.Join(home, "Pictures"))
		add("Desktop", filepath.Join(home, "Desktop"))
		add("Documents", filepath.Join(home, "Documents"))
		add("Home", home)
	}
	for _, entry := range platformBrowseRoots() {
		add(entry.Name, entry.Path)
	}
	if roots == nil {
		roots = []BrowseEntry{}
	}
	return roots
}

func browseFolders(rawPath string) (BrowseResponse, error) {
	roots := quickAccessRoots()
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		entries := make([]BrowseEntry, len(roots))
		copy(entries, roots)
		return BrowseResponse{
			Path:    "",
			Parent:  "",
			Roots:   roots,
			Entries: entries,
		}, nil
	}

	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return BrowseResponse{}, fmt.Errorf("invalid path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return BrowseResponse{}, fmt.Errorf("folder not accessible: %w", err)
	}
	if !info.IsDir() {
		return BrowseResponse{}, fmt.Errorf("%s is not a directory", abs)
	}

	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		return BrowseResponse{}, fmt.Errorf("read folder: %w", err)
	}

	entries := make([]BrowseEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		name := de.Name()
		if shouldSkipBrowseEntry(name) {
			continue
		}
		full := filepath.Join(abs, name)
		isDir := de.IsDir()
		if !isDir && de.Type()&os.ModeSymlink != 0 {
			if targetInfo, serr := os.Stat(full); serr == nil && targetInfo.IsDir() {
				isDir = true
			}
		}
		if !isDir {
			continue
		}
		entries = append(entries, BrowseEntry{Name: name, Path: full})
	}

	sort.Slice(entries, func(i, j int) bool {
		li, lj := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if li != lj {
			return li < lj
		}
		return entries[i].Name < entries[j].Name
	})
	if len(entries) > maxBrowseEntries {
		entries = entries[:maxBrowseEntries]
	}

	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	return BrowseResponse{
		Path:    abs,
		Parent:  parent,
		Roots:   roots,
		Entries: entries,
	}, nil
}

func shouldSkipBrowseEntry(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return true
	}
	switch strings.ToLower(name) {
	case "$recycle.bin", "system volume information", "recovery", "config.msi":
		return true
	}
	return false
}
