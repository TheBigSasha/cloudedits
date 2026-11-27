package service

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type launchd struct {
	dir string
	uid int
	run Runner
}

func (l *launchd) DefaultName() string { return DefaultLabel }

func (l *launchd) plistPath(name string) string { return filepath.Join(l.dir, name+".plist") }
func (l *launchd) domain() string               { return "gui/" + strconv.Itoa(l.uid) }
func (l *launchd) target(name string) string    { return l.domain() + "/" + name }

// LaunchdPlist renders a LaunchAgent that starts at login, restarts the
// server whenever it exits with an error (30 s apart, so a server waiting on
// an admin-console change retries without spinning), and leaves it stopped
// after a clean exit.
func LaunchdPlist(spec Spec) []byte {
	var b strings.Builder
	esc := func(s string) string {
		var e strings.Builder
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + esc(spec.Name) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range append([]string{spec.Executable}, spec.Args...) {
		b.WriteString("    <string>" + esc(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>30</integer>
`)
	if spec.WorkingDir != "" {
		b.WriteString("  <key>WorkingDirectory</key>\n  <string>" + esc(spec.WorkingDir) + "</string>\n")
	}
	if spec.LogPath != "" {
		b.WriteString("  <key>StandardOutPath</key>\n  <string>" + esc(spec.LogPath) + "</string>\n")
		b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + esc(spec.LogPath) + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

func (l *launchd) Install(ctx context.Context, spec Spec) (Status, error) {
	if err := ensureSpec(spec); err != nil {
		return Status{}, err
	}
	if spec.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
			return Status{}, err
		}
	}
	path := l.plistPath(spec.Name)
	if err := writeFileAtomic(path, LaunchdPlist(spec), 0o644); err != nil {
		return Status{}, err
	}
	// Unload any previous definition so the new one takes effect; "not
	// loaded" is the expected answer on a first install.
	_, _, _ = l.run(ctx, "launchctl", "bootout", l.target(spec.Name))
	out, code, err := l.run(ctx, "launchctl", "bootstrap", l.domain(), path)
	if err != nil || code != 0 {
		return Status{}, commandError("launchctl bootstrap", out, code, err)
	}
	return l.Status(ctx, spec.Name)
}

func (l *launchd) Uninstall(ctx context.Context, name string) (Status, error) {
	if _, _, err := l.run(ctx, "launchctl", "bootout", l.target(name)); err != nil {
		return Status{}, commandError("launchctl bootout", "", 0, err)
	}
	if err := os.Remove(l.plistPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
	}
	return l.Status(ctx, name)
}

func (l *launchd) Status(ctx context.Context, name string) (Status, error) {
	st := Status{Manager: "launchd", Name: name, UnitPath: l.plistPath(name)}
	if _, err := os.Stat(st.UnitPath); err == nil {
		st.Installed = true
	}
	out, code, err := l.run(ctx, "launchctl", "print", l.target(name))
	if err != nil {
		return st, commandError("launchctl print", "", 0, err)
	}
	if code == 0 {
		st.Loaded = true
		st.Detail = launchdSummary(out)
	} else {
		st.Detail = "not loaded"
	}
	return st, nil
}

// launchdSummary extracts state, pid and last exit code from launchctl print.
func launchdSummary(out string) string {
	var parts []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		for _, key := range []string{"state = ", "pid = ", "last exit code = "} {
			if strings.HasPrefix(line, key) {
				parts = append(parts, line)
			}
		}
	}
	if len(parts) == 0 {
		return "loaded"
	}
	return fmt.Sprint(strings.Join(parts, ", "))
}
