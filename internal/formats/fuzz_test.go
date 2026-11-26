package formats

import (
	"strings"
	"testing"
)

// FuzzFileClassification checks that filename classification never panics and
// stays internally consistent for arbitrary (including hostile) names.
func FuzzFileClassification(f *testing.F) {
	for _, s := range []string{
		"IMG_0001.JPG", "a/b/c.CR3", "._IMG_0001.JPG", ".hdrtoys", ".DS_Store", "Thumbs.db",
		"x.jpg/", "x.jpg\x00.png", "..\\..\\a.dng", "x.", ".jpg", "", "/", "a.JpEg", "photo.tar.gz",
	} {
		f.Add(s)
	}
	known := map[string]bool{}
	for _, ext := range SupportedExtensions() {
		known[ext] = true
	}
	f.Fuzz(func(t *testing.T, name string) {
		supported := IsSupportedFile(name)
		raw := IsRAW(name)
		mime := MIME(name)
		kind := Kind(name)
		if raw && !supported {
			t.Fatalf("%q is RAW but not supported", name)
		}
		if ShouldSkipName(name) && (supported || raw) {
			t.Fatalf("%q must be skipped but is classified as media", name)
		}
		if supported && mime == "application/octet-stream" {
			t.Fatalf("%q is supported but has no MIME type", name)
		}
		if supported && !strings.HasPrefix(mime, "image/") {
			t.Fatalf("%q has non-image MIME %q", name, mime)
		}
		if (kind == "raw") != raw {
			t.Fatalf("%q kind %q disagrees with IsRAW=%v", name, kind, raw)
		}
		if strings.ContainsAny(mime, "\r\n") {
			t.Fatalf("%q produced a MIME with control characters", name)
		}
	})
}
