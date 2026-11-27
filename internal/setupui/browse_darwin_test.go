//go:build darwin

package setupui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestIsExcludedDarwinVolume(t *testing.T) {
	excluded := []string{
		"",
		".",
		".timemachine",
		"com.apple.TimeMachine.localsnapshots",
		"com.apple.MobileBackup",
		"COM.APPLE.TimeMachine.localsnapshots",
		"com.apple.os.update-1234",
	}
	for _, name := range excluded {
		if !isExcludedDarwinVolume(name) {
			t.Errorf("isExcludedDarwinVolume(%q) = false, want true", name)
		}
	}

	allowed := []string{
		"Macintosh HD",
		"Crucial X8",
		"Crucial X8 4TB",
		"Photos Backup",
		"Share-3",
		"apple-photos",
	}
	for _, name := range allowed {
		if isExcludedDarwinVolume(name) {
			t.Errorf("isExcludedDarwinVolume(%q) = true, want false", name)
		}
	}
}

func TestDarwinVolumesFromDirFiltersSystemSnapshots(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{
		"Macintosh HD",
		"Crucial X8",
		"com.apple.TimeMachine.localsnapshots",
		"com.apple.os.update-test",
		".hidden-vol",
	} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "not-a-dir"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := darwinVolumesFromDir(dir)
	want := []BrowseEntry{
		{Name: "Crucial X8", Path: filepath.Join(dir, "Crucial X8")},
		{Name: "Macintosh HD", Path: filepath.Join(dir, "Macintosh HD")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("darwinVolumesFromDir = %+v, want %+v", got, want)
	}
}
