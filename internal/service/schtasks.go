package service

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// Windows uses a Task Scheduler task rather than a Windows service: creating
// a service requires administrator rights, while any user may register a task
// that runs as themselves at their own logon, with their own (least)
// privileges and access to their own folders and network drives.
type schtasks struct {
	run    Runner
	tmpDir string
}

func (s *schtasks) DefaultName() string { return DefaultTaskName }

// windowsArg quotes one argument the way CommandLineToArgvW parses it.
func windowsArg(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\"") {
		return a
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range a {
		switch r {
		case '\\':
			slashes++
			continue
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			b.WriteByte('"')
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			b.WriteRune(r)
		}
		slashes = 0
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}

// TaskXML renders a Task Scheduler definition that starts the server at the
// user's logon with least privilege, never stops it for running long or on
// battery, and restarts it every minute after a failure.
func TaskXML(spec Spec) string {
	esc := func(s string) string {
		var e strings.Builder
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	args := make([]string, 0, len(spec.Args))
	for _, a := range spec.Args {
		args = append(args, windowsArg(a))
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>cloudedits photo library server</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
`)
	if spec.User != "" {
		b.WriteString("      <UserId>" + esc(spec.User) + "</UserId>\n")
	}
	b.WriteString(`    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
`)
	if spec.User != "" {
		b.WriteString("      <UserId>" + esc(spec.User) + "</UserId>\n")
	}
	b.WriteString(`      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(spec.Executable) + `</Command>
`)
	if len(args) > 0 {
		b.WriteString("      <Arguments>" + esc(strings.Join(args, " ")) + "</Arguments>\n")
	}
	if spec.WorkingDir != "" {
		b.WriteString("      <WorkingDirectory>" + esc(spec.WorkingDir) + "</WorkingDirectory>\n")
	}
	b.WriteString(`    </Exec>
  </Actions>
</Task>
`)
	return b.String()
}

// utf16File encodes a task definition the way schtasks /XML expects it:
// UTF-16LE with a byte-order mark, matching the encoding declaration.
func utf16File(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(u))
	out[0], out[1] = 0xFF, 0xFE
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

func (s *schtasks) Install(ctx context.Context, spec Spec) (Status, error) {
	if err := ensureSpec(spec); err != nil {
		return Status{}, err
	}
	f, err := os.CreateTemp(s.tmpDir, "cloudedits-task-*.xml")
	if err != nil {
		return Status{}, err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := f.Write(utf16File(TaskXML(spec))); err != nil {
		_ = f.Close()
		return Status{}, err
	}
	if err := f.Close(); err != nil {
		return Status{}, err
	}
	// Stop a running copy first so a reinstall starts the new definition.
	_, _, _ = s.run(ctx, "schtasks", "/End", "/TN", spec.Name)
	out, code, err := s.run(ctx, "schtasks", "/Create", "/TN", spec.Name, "/XML", path, "/F")
	if err != nil || code != 0 {
		return Status{}, commandError("schtasks /Create", out, code, err)
	}
	out, code, err = s.run(ctx, "schtasks", "/Run", "/TN", spec.Name)
	if err != nil || code != 0 {
		return Status{}, commandError("schtasks /Run", out, code, err)
	}
	return s.Status(ctx, spec.Name)
}

func (s *schtasks) Uninstall(ctx context.Context, name string) (Status, error) {
	st, err := s.Status(ctx, name)
	if err != nil || !st.Installed {
		return st, err
	}
	_, _, _ = s.run(ctx, "schtasks", "/End", "/TN", name)
	out, code, err := s.run(ctx, "schtasks", "/Delete", "/TN", name, "/F")
	if err != nil || code != 0 {
		return Status{}, commandError("schtasks /Delete", out, code, err)
	}
	return s.Status(ctx, name)
}

func (s *schtasks) Status(ctx context.Context, name string) (Status, error) {
	st := Status{Manager: "schtasks", Name: name, UnitPath: `\` + name}
	out, code, err := s.run(ctx, "schtasks", "/Query", "/TN", name, "/XML")
	if err != nil {
		return st, commandError("schtasks /Query", "", 0, err)
	}
	if code != 0 {
		st.Detail = "not registered"
		return st, nil
	}
	st.Installed = true
	// The XML is locale-independent, unlike the LIST/CSV output.
	st.Loaded = !strings.Contains(out, "<Enabled>false</Enabled>")
	st.Detail = "registered in Task Scheduler (" + filepath.Base(name) + ")"
	return st, nil
}
