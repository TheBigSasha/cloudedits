package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAppImageBuildPinsVersionsAndChecksums ensures the Linux AppImage packager
// never fetches unverified rolling "continuous" releases of appimagetool or
// type2-runtime.
func TestAppImageBuildPinsVersionsAndChecksums(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "packaging", "linux", "build-appimage.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	if strings.Contains(script, "/download/continuous/") {
		t.Fatal("build-appimage.sh still fetches unverified rolling /download/continuous/ releases")
	}
	for _, want := range []string{
		`APPIMAGETOOL_VERSION="`,
		`TYPE2_RUNTIME_VERSION="`,
		`verify_sha256 "$APPIMAGETOOL"`,
		`verify_sha256 "$runtime_file"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("build-appimage.sh missing %q", want)
		}
	}
	shaVars := []string{
		"APPIMAGETOOL_SHA256_x86_64",
		"APPIMAGETOOL_SHA256_aarch64",
		"TYPE2_RUNTIME_SHA256_x86_64",
		"TYPE2_RUNTIME_SHA256_aarch64",
	}
	for _, name := range shaVars {
		re := regexp.MustCompile(`(?m)^` + name + `="[0-9a-f]{64}"$`)
		if !re.MatchString(script) {
			t.Errorf("build-appimage.sh missing pinned 64-hex SHA-256 variable %s", name)
		}
	}
}

// TestWindowsMSIGuidedInstallerAndZeroConsoleLauncherContract verifies that
// Windows releases ship the guided WiX v5 installer (Minimal UI, Start Menu +
// Desktop shortcuts, Finish-screen immediate launch checked by default) and
// the zero-console cloudedits-setup.exe launcher for both amd64 and arm64.
func TestWindowsMSIGuidedInstallerAndZeroConsoleLauncherContract(t *testing.T) {
	readRel := func(parts ...string) string {
		t.Helper()
		full := filepath.Join(append([]string{"..", ".."}, parts...)...)
		raw, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("read %s: %v", full, err)
		}
		return string(raw)
	}

	wxs := readRel("packaging", "windows", "cloudedits.wxs")
	for _, want := range []string{
		`xmlns:ui="http://wixtoolset.org/schemas/v4/wxs/ui"`,
		`xmlns:util="http://wixtoolset.org/schemas/v4/wxs/util"`,
		`<ui:WixUI Id="WixUI_Minimal" />`,
		`Id="WixUILicenseRtf"`,
		`Id="CloudeditsSetupExe"`,
		`Target="[INSTALLFOLDER]cloudedits-setup.exe"`,
		`<StandardDirectory Id="DesktopFolder">`,
		`Icon="CloudeditsIcon.ico"`,
		`Id="WIXUI_EXITDIALOGOPTIONALCHECKBOX" Value="1"`,
		`Id="WIXUI_EXITDIALOGOPTIONALCHECKBOXTEXT" Value="Launch cloudedits Setup"`,
		`Id="WixShellExecTarget" Value="[#CloudeditsSetupExe]"`,
		`BinaryRef="Wix4UtilCA_$(sys.BUILDARCHSHORT)"`,
		`DllEntry="WixShellExec"`,
		`Dialog="ExitDialog"`,
	} {
		if !strings.Contains(wxs, want) {
			t.Errorf("cloudedits.wxs missing %q", want)
		}
	}

	ps1 := readRel("packaging", "windows", "build-msi.ps1")
	for _, want := range []string{
		`-ext WixToolset.UI.wixext`,
		`-ext WixToolset.Util.wixext`,
		`-H=windowsgui`,
		`./cmd/cloudedits-setup`,
		`SetupBinaryPath=`,
		`LicenseRtfPath=`,
		`0x8664`,
		`0xAA64`,
		`Assert-ZeroDevSymbols`,
	} {
		if !strings.Contains(ps1, want) {
			t.Errorf("build-msi.ps1 missing %q", want)
		}
	}

	releaseYML := readRel(".github", "workflows", "release.yml")
	for _, want := range []string{
		`WixToolset.UI.wixext/5.0.2`,
		`WixToolset.Util.wixext/5.0.2`,
	} {
		if !strings.Contains(releaseYML, want) {
			t.Errorf("release.yml missing %q", want)
		}
	}

	archives := readRel("packaging", "scripts", "build-archives.sh")
	for _, want := range []string{
		`-H=windowsgui`,
		`./cmd/cloudedits-setup`,
		`"cloudedits-setup.exe"`,
	} {
		if !strings.Contains(archives, want) {
			t.Errorf("build-archives.sh missing %q", want)
		}
	}
}
