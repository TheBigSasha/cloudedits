//go:build !windows

package main

import "os/exec"

func configureAppLaunchCmd(*exec.Cmd) {}
