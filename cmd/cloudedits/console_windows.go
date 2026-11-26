//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// releaseOwnConsole closes the console window Windows created for this
// process when it was started without one (for example by Task Scheduler at
// logon). A console shared with a shell, where more than one process is
// attached, is left alone.
func releaseOwnConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	list := make([]uint32, 2)
	n, _, _ := kernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&list[0])), uintptr(len(list)))
	if n == 1 {
		_, _, _ = kernel32.NewProc("FreeConsole").Call()
	}
}
