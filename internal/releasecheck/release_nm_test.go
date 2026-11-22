//go:build !dev_apis

// Package releasecheck verifies that the compiled default release binary
// contains zero dev_apis handler symbols via `go tool nm`.
package releasecheck

import (
	"os/exec"
	"path/filepath"
	"regexp"
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
