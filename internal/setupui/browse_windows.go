//go:build windows

package setupui

import (
	"fmt"
	"syscall"
)

func platformBrowseRoots() []BrowseEntry {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	mask, _, _ := kernel32.NewProc("GetLogicalDrives").Call()
	var out []BrowseEntry
	for i := uint(0); i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		drive := fmt.Sprintf("%c:\\", 'A'+i)
		out = append(out, BrowseEntry{Name: drive, Path: drive})
	}
	return out
}
