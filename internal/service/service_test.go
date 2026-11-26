package service

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

var testSpec = Spec{
	Name:       "toys.hdr.cloudedits.test",
	Executable: "/Applications/cloud edits.app/Contents/MacOS/cloudedits",
	Args:       []string{"serve", "--tsnet", "--data-dir", "/Users/a b/Library/Application Support/cloudedits & <co>"},
	LogPath:    "/Users/a b/Library/Application Support/cloudedits/logs/serve.log",
	WorkingDir: "/Users/a b",
}

// fakeRunner records commands and answers from a script keyed by the first
// two words of the command line.
type fakeRunner struct {
	calls   []string
	answers map[string]struct {
		out  string
		code int
	}
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, int, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	for prefix, a := range f.answers {
		if strings.HasPrefix(line, prefix) {
			return a.out, a.code, nil
		}
	}
	return "", 0, nil
}

func (f *fakeRunner) answer(prefix, out string, code int) {
	if f.answers == nil {
		f.answers = map[string]struct {
			out  string
			code int
		}{}
	}
	f.answers[prefix] = struct {
		out  string
		code int
	}{out, code}
}

func TestLaunchdPlistIsValidXMLWithExactArguments(t *testing.T) {
	raw := LaunchdPlist(testSpec)
	var doc struct {
		Dict struct {
			Items []struct {
				XMLName xml.Name
				Text    string   `xml:",chardata"`
				Strings []string `xml:"string"`
			} `xml:",any"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("plist is not XML: %v\n%s", err, raw)
	}
	var keys []string
	var progArgs []string
	items := doc.Dict.Items
	for i, it := range items {
		if it.XMLName.Local == "key" {
			keys = append(keys, it.Text)
			if it.Text == "ProgramArguments" && i+1 < len(items) {
				progArgs = items[i+1].Strings
			}
		}
	}
	want := append([]string{testSpec.Executable}, testSpec.Args...)
	if strings.Join(progArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ProgramArguments = %q, want %q", progArgs, want)
	}
	for _, k := range []string{"Label", "RunAtLoad", "KeepAlive", "ThrottleInterval", "StandardErrorPath", "WorkingDirectory"} {
		if !contains(keys, k) {
			t.Errorf("plist lacks %s: %s", k, raw)
		}
	}
	if !strings.Contains(string(raw), "<key>SuccessfulExit</key>\n    <false/>") {
		t.Errorf("KeepAlive must restart only after a failed exit:\n%s", raw)
	}
}

func TestSystemdUnitQuotesEveryWord(t *testing.T) {
	spec := testSpec
	spec.Args = append(spec.Args, `100%"quoted"\$HOME`)
	unit := string(SystemdUnit(spec))
	want := `ExecStart="/Applications/cloud edits.app/Contents/MacOS/cloudedits" "serve" "--tsnet" "--data-dir" "/Users/a b/Library/Application Support/cloudedits & <co>" "100%%\"quoted\"\\$$HOME"`
	if !strings.Contains(unit, want+"\n") {
		t.Fatalf("unit:\n%s\nwant line:\n%s", unit, want)
	}
	for _, line := range []string{"Restart=on-failure", "RestartSec=30", "WantedBy=default.target", "Type=simple"} {
		if !strings.Contains(unit, line+"\n") {
			t.Errorf("unit lacks %q:\n%s", line, unit)
		}
	}
}

func TestTaskXMLParsesAndRunsLeastPrivilegeAtOwnLogon(t *testing.T) {
	spec := Spec{
		Name:       "cloudedits",
		Executable: `C:\Program Files\cloudedits\cloudedits.exe`,
		Args:       []string{"serve", "--data-dir", `C:\Users\A B\AppData\Local\cloudedits\`, `say "hi"`},
		User:       `HOST\a&b`,
	}
	doc := TaskXML(spec)
	var task struct {
		Triggers struct {
			Logon struct {
				UserID string `xml:"UserId"`
			} `xml:"LogonTrigger"`
		} `xml:"Triggers"`
		Principal struct {
			UserID    string `xml:"UserId"`
			LogonType string `xml:"LogonType"`
			RunLevel  string `xml:"RunLevel"`
		} `xml:"Principals>Principal"`
		Settings struct {
			Limit string `xml:"ExecutionTimeLimit"`
			Batt  string `xml:"StopIfGoingOnBatteries"`
		} `xml:"Settings"`
		Exec struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments"`
		} `xml:"Actions>Exec"`
	}
	// encoding/xml refuses a UTF-16 declaration on a UTF-8 string; parse the body.
	body := strings.Replace(doc, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)
	if err := xml.Unmarshal([]byte(body), &task); err != nil {
		t.Fatalf("task XML: %v\n%s", err, doc)
	}
	if task.Triggers.Logon.UserID != `HOST\a&b` || task.Principal.UserID != `HOST\a&b` {
		t.Errorf("trigger/principal user = %q/%q", task.Triggers.Logon.UserID, task.Principal.UserID)
	}
	if task.Principal.LogonType != "InteractiveToken" || task.Principal.RunLevel != "LeastPrivilege" {
		t.Errorf("principal = %+v, want InteractiveToken/LeastPrivilege", task.Principal)
	}
	if task.Settings.Limit != "PT0S" || task.Settings.Batt != "false" {
		t.Errorf("settings = %+v", task.Settings)
	}
	if task.Exec.Command != spec.Executable {
		t.Errorf("command = %q", task.Exec.Command)
	}
	wantArgs := `serve --data-dir "C:\Users\A B\AppData\Local\cloudedits\\" "say \"hi\""`
	if task.Exec.Arguments != wantArgs {
		t.Errorf("arguments = %s, want %s", task.Exec.Arguments, wantArgs)
	}

	enc := utf16File(doc)
	if enc[0] != 0xFF || enc[1] != 0xFE || len(enc) != 2+2*len(utf16.Encode([]rune(doc))) {
		t.Fatalf("task file must be UTF-16LE with BOM")
	}
}

func TestLaunchdInstallUninstallIsIdempotent(t *testing.T) {
	home := t.TempDir()
	f := &fakeRunner{}
	f.answer("launchctl print", "", 113)
	m, err := newFor("darwin", home, "", 501, f.run)
	if err != nil {
		t.Fatal(err)
	}
	testSpec := testSpec
	testSpec.LogPath = filepath.Join(home, "logs", "serve.log")
	st, err := m.Status(context.Background(), testSpec.Name)
	if err != nil || st.Installed || st.Loaded {
		t.Fatalf("before install: %+v %v", st, err)
	}
	f.answer("launchctl print", "\tstate = running\n\tpid = 4242\n", 0)
	for i := 0; i < 2; i++ {
		st, err = m.Install(context.Background(), testSpec)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Installed || !st.Loaded || !strings.Contains(st.Detail, "pid = 4242") {
			t.Fatalf("install #%d: %+v", i+1, st)
		}
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", testSpec.Name+".plist")
	if got, _ := os.ReadFile(plist); string(got) != string(LaunchdPlist(testSpec)) {
		t.Fatalf("plist on disk differs from LaunchdPlist")
	}
	wantSeq := []string{
		"launchctl print gui/501/" + testSpec.Name,
		"launchctl bootout gui/501/" + testSpec.Name,
		"launchctl bootstrap gui/501 " + plist,
		"launchctl print gui/501/" + testSpec.Name,
	}
	for i, w := range wantSeq {
		if f.calls[i] != w {
			t.Fatalf("call %d = %q, want %q (all %q)", i, f.calls[i], w, f.calls)
		}
	}
	f.answer("launchctl print", "", 113)
	for i := 0; i < 2; i++ {
		st, err = m.Uninstall(context.Background(), testSpec.Name)
		if err != nil || st.Installed || st.Loaded {
			t.Fatalf("uninstall #%d: %+v %v", i+1, st, err)
		}
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatalf("plist still present after uninstall")
	}
}

func TestLaunchdBootstrapFailureIsReported(t *testing.T) {
	f := &fakeRunner{}
	f.answer("launchctl bootstrap", "Bootstrap failed: 5: Input/output error", 5)
	m, _ := newFor("darwin", t.TempDir(), "", 501, f.run)
	spec := testSpec
	spec.LogPath = ""
	if _, err := m.Install(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("err = %v", err)
	}
}

func TestSystemdInstallUninstall(t *testing.T) {
	cfgHome := t.TempDir()
	f := &fakeRunner{}
	f.answer("systemctl --user show", "UnitFileState=enabled\nActiveState=active\nSubState=running\nMainPID=99\n", 0)
	f.answer("loginctl show-user", "Linger=no\n", 0)
	m, _ := newFor("linux", "/home/u", cfgHome, 1000, f.run)
	spec := testSpec
	spec.Name = "cloudedits"
	st, err := m.Install(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(cfgHome, "systemd", "user", "cloudedits.service")
	if !st.Installed || !st.Loaded || st.UnitPath != unit || !strings.Contains(st.Detail, "MainPID=99") || !strings.Contains(st.Detail, "enable-linger") {
		t.Fatalf("status = %+v", st)
	}
	for _, w := range []string{"systemctl --user daemon-reload", "systemctl --user enable cloudedits.service", "systemctl --user restart cloudedits.service"} {
		if !contains(f.calls, w) {
			t.Errorf("missing %q in %q", w, f.calls)
		}
	}
	f.answer("systemctl --user show", "UnitFileState=\nActiveState=inactive\n", 0)
	for i := 0; i < 2; i++ {
		st, err = m.Uninstall(context.Background(), "cloudedits")
		if err != nil || st.Installed || st.Loaded {
			t.Fatalf("uninstall #%d: %+v %v", i+1, st, err)
		}
	}
	if n := count(f.calls, "systemctl --user disable --now cloudedits.service"); n != 1 {
		t.Fatalf("disable ran %d times, want 1 (second uninstall is a no-op)", n)
	}
}

func TestSchtasksInstallUninstall(t *testing.T) {
	f := &fakeRunner{}
	f.answer("schtasks /Query", "", 1)
	m, _ := newFor("windows", "C:/Users/u", "", -1, f.run)
	m.(*schtasks).tmpDir = t.TempDir()
	spec := Spec{Name: "cloudedits", Executable: filepath.Join(t.TempDir(), "cloudedits.exe"), Args: []string{"serve"}, User: `HOST\u`}
	f.answer("schtasks /Query", "<Task><Settings><Enabled>true</Enabled></Settings></Task>", 0)
	st, err := m.Install(context.Background(), spec)
	if err != nil || !st.Installed || !st.Loaded {
		t.Fatalf("install: %+v %v", st, err)
	}
	var create string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "schtasks /Create") {
			create = c
		}
	}
	if !strings.Contains(create, "/TN cloudedits /XML ") || !strings.HasSuffix(create, " /F") || !contains(f.calls, "schtasks /Run /TN cloudedits") {
		t.Fatalf("calls = %q", f.calls)
	}
	entries, _ := os.ReadDir(m.(*schtasks).tmpDir)
	if len(entries) != 0 {
		t.Fatalf("task XML left behind: %v", entries)
	}
	f.answer("schtasks /Query", "", 1)
	st, err = m.Uninstall(context.Background(), "cloudedits")
	if err != nil || st.Installed {
		t.Fatalf("uninstall: %+v %v", st, err)
	}
	if count(f.calls, "schtasks /Delete /TN cloudedits /F") != 0 {
		t.Fatalf("an unregistered task must not be deleted: %q", f.calls)
	}
}

func TestResolveExecutable(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got, err := ResolveExecutable("/usr/local/bin/cloudedits", env(nil)); err != nil || got != "/usr/local/bin/cloudedits" {
		t.Fatalf("plain: %q %v", got, err)
	}
	if _, err := ResolveExecutable("/var/folders/x/T/go-build123/b001/exe/cloudedits", env(nil)); err == nil {
		t.Fatal("a go build temp binary must be refused")
	}
	got, err := ResolveExecutable("/tmp/.mount_abc/usr/bin/cloudedits", env(map[string]string{"APPIMAGE": "/home/u/Apps/cloudedits.AppImage", "APPDIR": "/tmp/.mount_abc"}))
	if err != nil || got != "/home/u/Apps/cloudedits.AppImage" {
		t.Fatalf("AppImage: %q %v", got, err)
	}
}

func TestRunnerEnvironmentDropsSecrets(t *testing.T) {
	got := WithoutSecrets([]string{"PATH=/bin", "TS_AUTHKEY=tskey-auth-x", "ts_auth_key=y", "CLOUDEDITS_TOKEN=z", "HOME=/h"})
	if strings.Join(got, ",") != "PATH=/bin,HOME=/h" {
		t.Fatalf("got %q", got)
	}
}

func contains(list []string, s string) bool { return count(list, s) > 0 }
func count(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}
