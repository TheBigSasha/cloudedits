// Package qr renders low-density QR codes as UTF-8 terminal blocks.
package qr

import qrcode "github.com/skip2/go-qrcode"

// Terminal renders content as a compact UTF-8 half-block QR code suitable for terminals.
func Terminal(content string) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", err
	}
	return q.ToSmallString(false), nil
}
