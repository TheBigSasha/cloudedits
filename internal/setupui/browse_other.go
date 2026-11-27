//go:build !windows && !darwin

package setupui

func platformBrowseRoots() []BrowseEntry {
	return []BrowseEntry{
		{Name: "Media", Path: "/media"},
		{Name: "Mounts", Path: "/mnt"},
		{Name: "Root (/)", Path: "/"},
	}
}
