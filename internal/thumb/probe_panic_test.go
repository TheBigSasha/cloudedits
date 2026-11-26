package thumb

import (
	"image"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// A decoder panic while probing one file must not crash the library scan,
// which probes every file on the caller's goroutine.
func TestProbeImageHeaderSurvivesDecoderPanic(t *testing.T) {
	image.RegisterFormat("panicking-test-format", "PANICZZ!",
		func(io.Reader) (image.Image, error) { panic("decoder bug") },
		func(io.Reader) (image.Config, error) { panic("decoder bug") })

	path := filepath.Join(t.TempDir(), "hostile.jpg")
	if err := os.WriteFile(path, []byte("PANICZZ! hdrgm:Version"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, h, gain := ProbeImageHeader(path)
	if w != 0 || h != 0 || !gain {
		t.Fatalf("ProbeImageHeader = %d,%d,%v; want 0,0,true", w, h, gain)
	}
}
