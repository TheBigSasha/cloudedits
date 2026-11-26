package main

import (
	"strings"
	"testing"
)

func TestBrowserDoesNotInheritSecrets(t *testing.T) {
	got := withoutSecrets([]string{"PATH=/usr/bin", "TS_AUTHKEY=tskey-auth-secret", "CLOUDEDITS_TOKEN=tok", "ts_authkey=lower", "HOME=/home/u", "TS_AUTHKEY_FILE=/x", "EMPTY="})
	want := []string{"PATH=/usr/bin", "HOME=/home/u", "TS_AUTHKEY_FILE=/x", "EMPTY="}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("withoutSecrets = %q, want %q", got, want)
	}
}
