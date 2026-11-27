//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// launchedDirectlyFromDesktop reports whether Windows allocated a brand-new
// console exclusively for this process (e.g. double-clicking cloudedits.exe in
// File Explorer) rather than inheriting a shared terminal from PowerShell,
// cmd.exe, or a parent runner.
func launchedDirectlyFromDesktop() bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	list := make([]uint32, 2)
	n, _, _ := kernel32.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&list[0])), uintptr(len(list)))
	return n == 1
}

// releaseOwnConsole closes the console window Windows created for this
// process when it was started without one (for example by Task Scheduler at
// logon or double-clicking cloudedits.exe). A console shared with a shell,
// where more than one process is attached, is left alone.
func releaseOwnConsole() {
	if launchedDirectlyFromDesktop() {
		kernel32 := syscall.NewLazyDLL("kernel32.dll")
		_, _, _ = kernel32.NewProc("FreeConsole").Call()
	}
}
