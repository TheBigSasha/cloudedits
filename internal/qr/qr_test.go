package qr

import "testing"

func TestTerminal(t *testing.T) {
	term, err := Terminal("https://library.example.ts.net/h/Ab12Cd34")
	if err != nil || len(term) == 0 {
		t.Fatalf("Terminal QR failed: %v", err)
	}
}
