//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const appCreateNoWindow = 0x08000000

func configureAppLaunchCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: appCreateNoWindow,
		HideWindow:    true,
	}
}
