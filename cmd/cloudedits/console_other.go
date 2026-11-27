//go:build !windows

package main

func launchedDirectlyFromDesktop() bool { return false }

func releaseOwnConsole() {}
