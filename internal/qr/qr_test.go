package qr

import (
	"strings"
	"testing"
)

func TestTerminalAndSVG(t *testing.T) {
	url := "https://library.example.ts.net/h/Ab12Cd34"
	term, err := Terminal(url)
	if err != nil || len(term) == 0 {
		t.Fatalf("Terminal QR failed: %v", err)
	}
	svg, err := SVG(url)
	if err != nil {
		t.Fatalf("SVG QR failed: %v", err)
	}
	if !strings.HasPrefix(svg, "<svg ") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("unexpected SVG output: %q", svg[:min(len(svg), 80)])
	}
}
