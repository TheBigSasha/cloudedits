package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type systemd struct {
	dir string
	run Runner
}

func (s *systemd) DefaultName() string { return DefaultSystemdName }

func (s *systemd) unitPath(name string) string { return filepath.Join(s.dir, name+".service") }

// systemdQuote quotes one ExecStart word: double quotes with C escapes, and
// "%" doubled because systemd expands specifiers inside quotes too.
func systemdQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '%':
			b.WriteString("%%")
		case '$':
			b.WriteString("$$")
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// SystemdUnit renders a user unit that starts with the user's session (or at
// boot when lingering is enabled) and restarts the server 30 s after any
// failure.
func SystemdUnit(spec Spec) []byte {
	words := make([]string, 0, len(spec.Args)+1)
	for _, a := range append([]string{spec.Executable}, spec.Args...) {
		words = append(words, systemdQuote(a))
	}
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=cloudedits photo library server\n\n[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n")
	if spec.WorkingDir != "" {
		b.WriteString("WorkingDirectory=" + systemdQuote(spec.WorkingDir) + "\n")
	}
	b.WriteString("Restart=on-failure\nRestartSec=30\n\n[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

func (s *systemd) Install(ctx context.Context, spec Spec) (Status, error) {
	if err := ensureSpec(spec); err != nil {
		return Status{}, err
	}
	if err := writeFileAtomic(s.unitPath(spec.Name), SystemdUnit(spec), 0o644); err != nil {
		return Status{}, err
	}
	unit := spec.Name + ".service"
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", unit},
		// restart, not start: a reinstall must pick up a changed unit.
		{"--user", "restart", unit},
	} {
		out, code, err := s.run(ctx, "systemctl", args...)
		if err != nil || code != 0 {
			return Status{}, commandError("systemctl "+strings.Join(args, " "), out, code, err)
		}
	}
	return s.Status(ctx, spec.Name)
}

func (s *systemd) Uninstall(ctx context.Context, name string) (Status, error) {
	unit := name + ".service"
	if _, err := os.Stat(s.unitPath(name)); err == nil {
		out, code, err := s.run(ctx, "systemctl", "--user", "disable", "--now", unit)
		if err != nil || code != 0 {
			return Status{}, commandError("systemctl --user disable --now", out, code, err)
		}
		if err := os.Remove(s.unitPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Status{}, err
		}
		_, _, _ = s.run(ctx, "systemctl", "--user", "daemon-reload")
	}
	return s.Status(ctx, name)
}

func (s *systemd) Status(ctx context.Context, name string) (Status, error) {
	st := Status{Manager: "systemd", Name: name, UnitPath: s.unitPath(name)}
	if _, err := os.Stat(st.UnitPath); err == nil {
		st.Installed = true
	}
	out, _, err := s.run(ctx, "systemctl", "--user", "show", name+".service", "--property=UnitFileState,ActiveState,SubState,MainPID")
	if err != nil {
		return st, commandError("systemctl --user show", "", 0, err)
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	st.Loaded = props["UnitFileState"] == "enabled"
	var parts []string
	for _, k := range []string{"UnitFileState", "ActiveState", "SubState", "MainPID"} {
		if v := props[k]; v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	st.Detail = strings.Join(parts, ", ")
	if out, _, err := s.run(ctx, "loginctl", "show-user", "--property=Linger"); err == nil && strings.TrimSpace(out) == "Linger=no" {
		st.Detail += "; lingering is off, so the library runs only while you are logged in (enable it with: loginctl enable-linger)"
	}
	return st, nil
}
