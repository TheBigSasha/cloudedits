//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	createNoWindow = 0x08000000
	mbOK           = 0x00000000
	mbIconError    = 0x00000010
)

func launchHiddenGUI(exePath string, args []string) error {
	cmd := exec.Command(exePath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow,
		HideWindow:    true,
	}
	return cmd.Start()
}

func reportLaunchError(title, message string) {
	tPtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	mPtr, err := syscall.UTF16PtrFromString(message)
	if err != nil {
		return
	}
	user32 := syscall.NewLazyDLL("user32.dll")
	_, _, _ = user32.NewProc("MessageBoxW").Call(
		0,
		uintptr(unsafe.Pointer(mPtr)),
		uintptr(unsafe.Pointer(tPtr)),
		uintptr(mbOK|mbIconError),
	)
}
