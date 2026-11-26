package main

import (
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
)

func TestResolveTokenAuth(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg         config.Config
		flagToken   string
		flagRequire bool
		wantToken   string
		wantRequire bool
	}{
		"defaults":                    {config.Config{AuthMode: "tailnet"}, "", false, "", false},
		"flag token only":             {config.Config{AuthMode: "tailnet"}, " tok ", false, "tok", false},
		"flag require":                {config.Config{}, "tok", true, "tok", true},
		"config token mode":           {config.Config{AuthMode: "token", BearerToken: "cfg-tok"}, "", false, "cfg-tok", true},
		"config token mode, any case": {config.Config{AuthMode: " Token "}, "", false, "", true},
		"flag token beats config":     {config.Config{AuthMode: "token", BearerToken: "cfg-tok"}, "flag-tok", false, "flag-tok", true},
	} {
		tok, req := resolveTokenAuth(&tc.cfg, tc.flagToken, tc.flagRequire)
		if tok != tc.wantToken || req != tc.wantRequire {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", name, tok, req, tc.wantToken, tc.wantRequire)
		}
	}
}
