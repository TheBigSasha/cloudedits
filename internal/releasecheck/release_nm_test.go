//go:build !dev_apis

// Package releasecheck verifies that the compiled default release binary
// contains zero dev_apis handler symbols via `go tool nm`.
package releasecheck

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

func TestReleaseBinaryContainsZeroDevSymbols(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping release binary build in -short mode")
	}
	outBin := filepath.Join(t.TempDir(), "cloudedits-release")
	cmd := exec.Command("go", "build", "-trimpath", "-o", outBin, "../../cmd/cloudedits")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build release binary failed: %v\n%s", err, string(out))
	}

	nmCmd := exec.Command("go", "tool", "nm", outBin)
	nmOut, err := nmCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go tool nm failed: %v\n%s", err, string(nmOut))
	}

	forbidden := regexp.MustCompile(`(?i)(handleDevReset|handleDevFault|handleDevStats|devFaultConfig|faultReadSeeker|devapi)`)
	if match := forbidden.Find(nmOut); match != nil {
		t.Fatalf("forbidden dev_apis symbol leaked into release binary: %q", string(match))
	}
}

// The release gate must be able to fail. A dev_apis binary stripped with
// -s -w has no symbol table, so a gate built on `go tool nm` alone passes it;
// the packaging script has to reject it anyway.
func TestReleaseGateRejectsStrippedDevBuild(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("builds two binaries and needs a POSIX shell")
	}
	dir := t.TempDir()
	script := filepath.Join("..", "..", "packaging", "scripts", "check-release-symbols.sh")
	build := func(name string, extra ...string) string {
		t.Helper()
		out := filepath.Join(dir, name)
		args := append([]string{"build", "-trimpath", "-ldflags=-s -w"}, extra...)
		args = append(args, "-o", out, "../../cmd/cloudedits")
		if b, err := exec.Command("go", args...).CombinedOutput(); err != nil {
			t.Fatalf("go %v failed: %v\n%s", args, err, b)
		}
		return out
	}

	if out, err := exec.Command("bash", script, build("release")).CombinedOutput(); err != nil {
		t.Fatalf("gate rejected a clean stripped release build: %v\n%s", err, out)
	}
	if out, err := exec.Command("bash", script, build("dev", "-tags", "dev_apis")).CombinedOutput(); err == nil {
		t.Fatalf("gate accepted a stripped dev_apis build:\n%s", out)
	}
}
