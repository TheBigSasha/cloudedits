package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thebigsasha/cloudedits/internal/api"
	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/runstate"
	"github.com/thebigsasha/cloudedits/internal/service"
	"github.com/thebigsasha/cloudedits/internal/setupui"
	"github.com/thebigsasha/cloudedits/internal/tsnetnode"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

// Environment for re-executing the test binary as the cloudedits CLI, the
// way a service manager would start it.
const (
	envCLIArgs    = "CLOUDEDITS_TEST_CLI_ARGS"
	envControlURL = "CLOUDEDITS_TEST_CONTROL_URL"
)

func TestMain(m *testing.M) {
	if raw := os.Getenv(envCLIArgs); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitUsage)
		}
		tsnetTestHooks.ControlURL = os.Getenv(envControlURL)
		tsnetTestHooks.Logf = logger.Discard
		tsnetTestHooks.CertPair = noTestCerts
		os.Exit(run(args))
	}
	// No test may register a real launchd/systemd/Task Scheduler service.
	newServiceManager = func() (service.Manager, error) { return &fakeServices{}, nil }
	executablePath = func() (string, error) { return "/opt/cloudedits/bin/cloudedits", nil }
	os.Exit(m.Run())
}

// fakeServices stands in for the OS service manager. With spawn set, Install
// starts the service's command line as a child process of the test binary,
// as launchd or systemd would, and Uninstall stops it.
type fakeServices struct {
	mu         sync.Mutex
	spawn      bool
	controlURL string
	installed  map[string]service.Spec
	procs      map[string]*exec.Cmd
	logs       map[string]*lockedBuffer
}

// lockedBuffer collects a child's output, which exec copies on its own
// goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (f *fakeServices) DefaultName() string { return "cloudedits-test" }

func (f *fakeServices) Install(_ context.Context, spec service.Spec) (service.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installed == nil {
		f.installed, f.procs, f.logs = map[string]service.Spec{}, map[string]*exec.Cmd{}, map[string]*lockedBuffer{}
	}
	f.stopLocked(spec.Name)
	f.installed[spec.Name] = spec
	if f.spawn {
		args, _ := json.Marshal(spec.Args)
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(service.WithoutSecrets(os.Environ()), envCLIArgs+"="+string(args), envControlURL+"="+f.controlURL)
		buf := &lockedBuffer{}
		cmd.Stdout, cmd.Stderr = buf, buf
		if err := cmd.Start(); err != nil {
			return service.Status{}, err
		}
		f.procs[spec.Name], f.logs[spec.Name] = cmd, buf
	}
	return service.Status{Manager: "fake", Name: spec.Name, Installed: true, Loaded: true}, nil
}

func (f *fakeServices) stopLocked(name string) {
	if cmd := f.procs[name]; cmd != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		delete(f.procs, name)
	}
}

func (f *fakeServices) Uninstall(_ context.Context, name string) (service.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopLocked(name)
	delete(f.installed, name)
	return service.Status{Manager: "fake", Name: name}, nil
}

func (f *fakeServices) Status(_ context.Context, name string) (service.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.installed[name]
	return service.Status{Manager: "fake", Name: name, Installed: ok, Loaded: ok}, nil
}

func (f *fakeServices) spec(name string) (service.Spec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.installed[name]
	return s, ok
}

func (f *fakeServices) log(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b := f.logs[name]; b != nil {
		return b.String()
	}
	return ""
}

func useServices(t *testing.T, f *fakeServices) {
	t.Helper()
	prev := newServiceManager
	newServiceManager = func() (service.Manager, error) { return f, nil }
	t.Cleanup(func() {
		for name := range f.installed {
			_, _ = f.Uninstall(context.Background(), name)
		}
		newServiceManager = prev
	})
}

// noTestCerts stands in for the certificate fetch, which would otherwise ask
// Let's Encrypt over the internet for a certificate the test coordination
// server cannot prove.
func noTestCerts(context.Context, string) ([]byte, []byte, error) {
	return nil, nil, errors.New("the test coordination server cannot issue certificates")
}

// skipOnTestBuild skips tests that serve over the tailnet, which a dev_apis
// build refuses to do (see checkTestBuildExposure).
func skipOnTestBuild(t *testing.T) {
	t.Helper()
	if api.HasTestRoutes() {
		t.Skip("dev_apis builds refuse to serve on the tailnet")
	}
}

const flowKey = "tskey-auth-kFLOWTEST-fedcba9876543210"

// startControl runs an in-process coordination server. It names nodes by bare
// host label and serves no MagicDNS records, so the record a real tailnet
// serves for the first node (100.64.0.1) is supplied as an extra record.
func startControl(t *testing.T, requireKey string) *testcontrol.Server {
	t.Helper()
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	derpMap := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	control := &testcontrol.Server{
		DERPMap:        derpMap,
		RequireAuthKey: requireKey,
		MagicDNSDomain: "tail-scale.ts.net",
		DNSConfig: &tailcfg.DNSConfig{Proxied: true, ExtraRecords: []tailcfg.DNSRecord{
			{Name: "library.tail-scale.ts.net", Type: "A", Value: "100.64.0.1"},
		}},
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	prev := tsnetTestHooks
	tsnetTestHooks.ControlURL, tsnetTestHooks.Logf, tsnetTestHooks.CertPair = control.HTTPTestServer.URL, logger.Discard, noTestCerts
	t.Cleanup(func() { tsnetTestHooks = prev })
	return control
}

// libraryDir returns a data dir whose config serves one photo folder on an
// ephemeral loopback port.
func libraryDir(t *testing.T) (dataDir, photos string) {
	t.Helper()
	dataDir, photos = t.TempDir(), t.TempDir()
	writeConfigJSON(t, dataDir, `{"libraryId":"lib-flow","hostname":"library","listenAddr":"127.0.0.1:0","folders":[]}`)
	return dataDir, photos
}

func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	go func() { _, _ = io.WriteString(w, input); _ = w.Close() }()
	defer func() { os.Stdin = orig; _ = r.Close() }()
	fn()
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	os.Stderr = orig
	_ = w.Close()
	return <-done
}

// Before this change `setup --install-service` printed serviceInstalled:false
// and nothing kept the library running. It now registers the service through
// the service manager, which starts serve, and reports the running server.
func TestSetupInstallServiceStartsTheServer(t *testing.T) {
	isolateDefaultDataDir(t)
	svcs := &fakeServices{spawn: true}
	useServices(t, svcs)
	dataDir, photos := libraryDir(t)

	code, out := runJSON(t, "setup", "--data-dir", dataDir, "--folder", photos, "--install-service", "--json")
	if code != exitOK || out["serviceInstalled"] != true {
		t.Fatalf("setup: code=%d out=%v\nserver log: %s", code, out, svcs.log("cloudedits-test"))
	}
	spec, ok := svcs.spec("cloudedits-test")
	if !ok || spec.Executable != "/opt/cloudedits/bin/cloudedits" || strings.Join(spec.Args, " ") != "serve --data-dir "+dataDir {
		t.Fatalf("installed spec = %+v", spec)
	}
	server, _ := out["server"].(map[string]any)
	if server["running"] != true || server["state"] != "serving" || !strings.HasPrefix(server["url"].(string), "http://127.0.0.1:") {
		t.Fatalf("server = %v", server)
	}

	// doctor sees the running server and probes its listener; the only
	// failing check is that the library has not joined a tailnet.
	code, doc := runJSON(t, "doctor", "--data-dir", dataDir, "--json")
	byName := doctorChecks(t, doc)
	if code != exitActionRequired {
		t.Fatalf("doctor exit = %d, want 3: %v", code, doc)
	}
	for name, want := range map[string]string{"server": "ok", "listener": "ok", "service": "ok", "index": "ok", "tailscale": "fail"} {
		if byName[name]["status"] != want {
			t.Errorf("doctor %s = %v, want %s", name, byName[name], want)
		}
	}

	if code, _ := runJSON(t, "service", "uninstall", "--data-dir", dataDir, "--json"); code != exitOK {
		t.Fatalf("service uninstall exited %d", code)
	}
	if live := runstate.Observe(dataDir); live.Running || live.State != runstate.StateStopped {
		t.Fatalf("after uninstall: %+v", live)
	}
}

func doctorChecks(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, _ := doc["checks"].([]any)
	for _, c := range list {
		m := c.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

// The whole agent-driven CLI flow against a real tsnet node: the key is read
// from stdin, the node joins, the installed service serves over the tailnet,
// and the key appears in no output, file, service definition or child
// environment.
func TestSetupEnrolsAndServesOverTheTailnetWithoutLeakingTheKey(t *testing.T) {
	skipOnTestBuild(t)
	isolateDefaultDataDir(t)
	control := startControl(t, "")
	svcs := &fakeServices{spawn: true, controlURL: control.HTTPTestServer.URL}
	useServices(t, svcs)
	dataDir, photos := libraryDir(t)

	var code int
	var stdout string
	stderr := captureStderr(t, func() {
		withStdin(t, flowKey+"\n", func() {
			stdout = captureStdout(t, func() {
				code = run([]string{"setup", "--data-dir", dataDir, "--folder", photos, "--tailscale-auth-key-stdin", "--install-service", "--json"})
			})
		})
	})
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout: %v %q", err, stdout)
	}
	server, _ := out["server"].(map[string]any)
	if out["url"] != "https://library.tail-scale.ts.net" || out["enrolled"] != true || server["state"] != "serving" || server["mode"] != "tsnet" {
		t.Fatalf("setup: code=%d out=%v\nserver log: %s", code, out, svcs.log("cloudedits-test"))
	}
	spec, _ := svcs.spec("cloudedits-test")
	if strings.Join(spec.Args, " ") != "serve --tsnet --data-dir "+dataDir {
		t.Fatalf("service args = %q", spec.Args)
	}

	// The server's own checks land in the status file; MagicDNS resolves.
	// (This coordination server cannot issue certificates, so the HTTPS
	// check is expected to fail here.)
	deadline := time.Now().Add(2 * time.Minute)
	var live runstate.Live
	for time.Now().Before(deadline) {
		if live = runstate.Observe(dataDir); live.MagicDNS != nil && live.HTTPS != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if live.MagicDNS == nil || !live.MagicDNS.OK || live.HTTPS == nil || live.HTTPS.OK {
		t.Fatalf("self-checks: magicDns=%+v https=%+v\nserver log: %s", live.MagicDNS, live.HTTPS, svcs.log("cloudedits-test"))
	}
	dcode, doc := runJSON(t, "doctor", "--data-dir", dataDir, "--json")
	checks := doctorChecks(t, doc)
	if dcode != exitActionRequired || checks["tailscale"]["status"] != "ok" || checks["server"]["status"] != "ok" ||
		checks["magicdns"]["status"] != "ok" || checks["https_cert"]["status"] != "fail" {
		t.Fatalf("doctor: exit=%d %v", dcode, doc)
	}

	leaks := map[string]string{"stdout": stdout, "stderr": stderr, "server log": svcs.log("cloudedits-test"),
		"service args": strings.Join(spec.Args, " "), "service executable": spec.Executable}
	_ = filepath.Walk(dataDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			leaks[p] = string(b)
		}
		return nil
	})
	for where, text := range leaks {
		if strings.Contains(text, flowKey) || strings.Contains(text, "kFLOWTEST") {
			t.Errorf("the auth key leaked into %s", where)
		}
	}
	if code, _ := runJSON(t, "service", "uninstall", "--data-dir", dataDir, "--json"); code != exitOK {
		t.Fatalf("uninstall exited %d", code)
	}
}

func TestSetupRejectsAWrongKeyAndKeepsTheLibraryUnenrolled(t *testing.T) {
	isolateDefaultDataDir(t)
	startControl(t, flowKey)
	dataDir, photos := libraryDir(t)
	var code int
	var out map[string]any
	withStdin(t, "tskey-auth-wrong\n", func() {
		code, out = runJSON(t, "setup", "--data-dir", dataDir, "--folder", photos, "--tailscale-auth-key-stdin", "--json")
	})
	if code != exitActionRequired || !strings.Contains(fmt.Sprint(out["error"]), "invalid authkey") {
		t.Fatalf("wrong key: code=%d out=%v", code, out)
	}
	if strings.Contains(fmt.Sprint(out), "tskey-auth-wrong") {
		t.Fatalf("the key is echoed: %v", out)
	}
	if args := serveArgs(dataDir, "linux"); strings.Contains(strings.Join(args, " "), "--tsnet") {
		t.Fatalf("an unenrolled library must not get a tailnet service: %q", args)
	}
}

func TestEnrolmentRefusesWhileAServerOwnsTheDataDir(t *testing.T) {
	dataDir, _ := libraryDir(t)
	lock, err := runstate.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	cfg, _ := config.Load(dataDir)
	if _, err := enrollNode(context.Background(), cfg, flowKey); !errors.Is(err, errServerRunning) {
		t.Fatalf("enrol while owned: %v", err)
	}
}

func TestServeArgsPerPlatform(t *testing.T) {
	dataDir := t.TempDir()
	if got := strings.Join(serveArgs(dataDir, "darwin"), " "); got != "serve --data-dir "+dataDir {
		t.Fatalf("unenrolled darwin: %q", got)
	}
	state := tsnetnode.StateDir(dataDir)
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Node state alone (left behind by a rejected key) is not enrolment.
	if got := strings.Join(serveArgs(dataDir, "linux"), " "); got != "serve --data-dir "+dataDir {
		t.Fatalf("state without a recorded address: %q", got)
	}
	if err := recordNodeIdentity(dataDir, "library.example.ts.net", "100.64.0.9"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(serveArgs(dataDir, "linux"), " "); got != "serve --tsnet --data-dir "+dataDir {
		t.Fatalf("enrolled linux: %q", got)
	}
	want := "serve --tsnet --data-dir " + dataDir + " --log-file " + filepath.Join(dataDir, "logs", "serve.log")
	if got := strings.Join(serveArgs(dataDir, "windows"), " "); got != want {
		t.Fatalf("enrolled windows: %q", got)
	}
}

func TestDoctorExitCodes(t *testing.T) {
	isolateDefaultDataDir(t)
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	// Nothing configured: action required.
	empty := t.TempDir()
	code, doc := runJSON(t, "doctor", "--data-dir", empty, "--json")
	checks := doctorChecks(t, doc)
	if code != exitActionRequired || checks["folders"]["status"] != "fail" || checks["tailscale"]["status"] != "fail" || checks["server"]["status"] != "fail" {
		t.Fatalf("empty: exit=%d %v", code, doc)
	}

	// A folder whose sidecar directory cannot be created is a storage
	// problem, which outranks the action-required findings.
	dataDir, photos := libraryDir(t)
	if code, _ := runJSON(t, "folders", "add", photos, "--data-dir", dataDir, "--json"); code != exitOK {
		t.Fatal("folders add")
	}
	if err := os.Chmod(photos, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(photos, 0o755) })
	code, doc = runJSON(t, "doctor", "--data-dir", dataDir, "--json")
	cfg, _ := config.Load(dataDir)
	folder := doctorChecks(t, doc)["folder:"+cfg.Folders[0].ID]
	if code != exitStorage || folder["status"] != "fail" || !strings.Contains(folder["detail"].(string), "not writable") {
		t.Fatalf("read-only folder: exit=%d %v", code, doc)
	}

	// An unreadable folder is a storage problem too.
	if err := os.Chmod(photos, 0o000); err != nil {
		t.Fatal(err)
	}
	code, doc = runJSON(t, "doctor", "--data-dir", dataDir, "--json")
	folder = doctorChecks(t, doc)["folder:"+cfg.Folders[0].ID]
	if code != exitStorage || !strings.Contains(folder["detail"].(string), "cannot read") {
		t.Fatalf("unreadable folder: exit=%d %v", code, doc)
	}
}

// With every check green doctor exits 0. The server side is simulated by
// holding the data-dir lock and writing the status a healthy tailnet server
// records, which is exactly what doctor reads.
func TestDoctorPassesForAHealthyTailnetServer(t *testing.T) {
	isolateDefaultDataDir(t)
	useServices(t, &fakeServices{})
	if _, _, err := installService(context.Background(), "", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	dataDir, photos := libraryDir(t)
	if code, _ := runJSON(t, "folders", "add", photos, "--data-dir", dataDir, "--json"); code != exitOK {
		t.Fatal("folders add")
	}
	state := tsnetnode.StateDir(dataDir)
	_ = os.MkdirAll(state, 0o700)
	_ = os.WriteFile(filepath.Join(state, "tailscaled.state"), []byte("{}"), 0o600)
	_ = recordNodeIdentity(dataDir, "library.example.ts.net", "100.64.0.9")
	lock, err := runstate.Acquire(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	now := time.Now().UTC()
	exp := now.Add(80 * 24 * time.Hour)
	_ = runstate.WriteStatus(dataDir, runstate.Status{PID: os.Getpid(), State: runstate.StateServing, Mode: "tsnet",
		URL: "https://library.example.ts.net", FQDN: "library.example.ts.net", StartedAt: now,
		MagicDNS: &runstate.Check{OK: true, Detail: "resolves", CheckedAt: now}, HTTPS: &runstate.Check{OK: true, Detail: "valid", CheckedAt: now}, CertNotAfter: &exp})
	code, doc := runJSON(t, "doctor", "--data-dir", dataDir, "--json")
	if code != exitOK || doc["ok"] != true {
		t.Fatalf("healthy: exit=%d %v", code, doc)
	}
	if d := doctorChecks(t, doc)["https_cert"]["detail"].(string); !strings.Contains(d, exp.Format("2006-01-02")) {
		t.Fatalf("https detail = %q", d)
	}

	// A failed HTTPS self-check is an action for the user.
	_ = runstate.WriteStatus(dataDir, runstate.Status{PID: os.Getpid(), State: runstate.StateServing, Mode: "tsnet", FQDN: "library.example.ts.net", StartedAt: now,
		MagicDNS: &runstate.Check{OK: true, CheckedAt: now}, HTTPS: &runstate.Check{OK: false, Detail: "acme: rate limited", CheckedAt: now}})
	code, doc = runJSON(t, "doctor", "--data-dir", dataDir, "--json")
	if https := doctorChecks(t, doc)["https_cert"]; code != exitActionRequired || https["fix"] != actionEnableHTTPS {
		t.Fatalf("https failing: exit=%d %v", code, doc)
	}
}

// The wizard's Connect goes through the same enrolment and service install
// as the CLI. A rejected key leaves the library unenrolled with no service.
func TestWizardConnectRejectsAWrongKey(t *testing.T) {
	isolateDefaultDataDir(t)
	startControl(t, flowKey)
	svcs := &fakeServices{}
	useServices(t, svcs)
	dataDir, _ := libraryDir(t)
	g := &guiState{dataDir: dataDir}
	ctx := context.Background()
	if _, err := g.Connect(ctx, "tskey-auth-wrong", func(setupui.Phase) {}); err == nil || !strings.Contains(err.Error(), "invalid authkey") {
		t.Fatalf("wrong key: %v", err)
	}
	if st := g.Server(ctx); st.Enrolled || st.ServiceInstalled {
		t.Fatalf("after a wrong key: %+v", st)
	}
}

func TestWizardConnectEnrolsAndStartsTheService(t *testing.T) {
	skipOnTestBuild(t)
	isolateDefaultDataDir(t)
	control := startControl(t, "")
	svcs := &fakeServices{spawn: true, controlURL: control.HTTPTestServer.URL}
	useServices(t, svcs)
	dataDir, photos := libraryDir(t)
	if code, _ := runJSON(t, "folders", "add", photos, "--data-dir", dataDir, "--json"); code != exitOK {
		t.Fatal("folders add")
	}
	g := &guiState{dataDir: dataDir}
	ctx := context.Background()

	var phases []setupui.Phase
	res, err := g.Connect(ctx, flowKey, func(p setupui.Phase) { phases = append(phases, p) })
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if res.ActionRequired != "" || fmt.Sprint(phases) != "[joining starting]" {
		t.Fatalf("result %+v phases %v", res, phases)
	}
	live := waitForServer(ctx, dataDir, 60*time.Second)
	st := g.Server(ctx)
	if !st.Enrolled || !st.ServiceInstalled || !st.Running || st.State != "serving" || st.URL != "https://library.tail-scale.ts.net" || g.PublicURL() != st.URL {
		t.Fatalf("server state %+v (live %+v)\nlog: %s", st, live, svcs.log("cloudedits-test"))
	}
}
