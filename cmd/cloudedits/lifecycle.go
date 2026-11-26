package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/index"
	"github.com/thebigsasha/cloudedits/internal/runstate"
	"github.com/thebigsasha/cloudedits/internal/service"
	"github.com/thebigsasha/cloudedits/internal/tsnetnode"
	"tailscale.com/types/logger"
)

// tsnetTestHooks points every tsnet node this binary starts at an in-process
// coordination server. Only tests set it; no flag or environment variable
// does.
var tsnetTestHooks struct {
	ControlURL string
	Logf       logger.Logf
	CertPair   func(ctx context.Context, domain string) (certPEM, keyPEM []byte, err error)
}

func tsnetOptions(cfg *config.Config, authKey string) tsnetnode.Options {
	return tsnetnode.Options{
		Hostname:   cfg.Hostname,
		StateDir:   tsnetnode.StateDir(cfg.DataDir),
		AuthKey:    authKey,
		ControlURL: tsnetTestHooks.ControlURL,
		Logf:       tsnetTestHooks.Logf,
		CertPair:   tsnetTestHooks.CertPair,
	}
}

// enrollResult is what joining the tailnet produced.
type enrollResult struct {
	FQDN        string
	TailscaleIP string
	// HTTPSEnabled is false when the tailnet has MagicDNS or HTTPS
	// certificates turned off; the node is enrolled either way.
	HTTPSEnabled bool
}

// errServerRunning reports that a server owns the data directory, so its
// tailnet identity cannot be used by a second node at the same time.
var errServerRunning = errors.New("the library server is running from this data directory; stop it first (cloudedits service uninstall, or stop the serve process)")

// enrollNode joins the tailnet with authKey and records the node's address in
// the config. It is the only enrolment path: setup and the setup wizard both
// call it, and serve brings the same node up from its saved state. The key is
// handed to tsnet in memory only. The node is shut down again before
// returning, so whoever serves next owns it alone.
func enrollNode(ctx context.Context, cfg *config.Config, authKey string) (enrollResult, error) {
	lock, err := runstate.Acquire(cfg.DataDir)
	if errors.Is(err, runstate.ErrLocked) {
		return enrollResult{}, errServerRunning
	}
	if err != nil {
		return enrollResult{}, err
	}
	defer func() { _ = lock.Release() }()

	node, err := tsnetnode.Start(ctx, tsnetOptions(cfg, authKey))
	var res enrollResult
	if node != nil {
		res.FQDN, res.TailscaleIP = node.FQDN(), node.TailscaleIP()
		_ = node.Close()
	}
	httpsOff := errors.Is(err, tsnetnode.ErrHTTPSNotEnabled)
	if err != nil && !httpsOff {
		return enrollResult{}, redactKey(err, authKey)
	}
	res.HTTPSEnabled = !httpsOff
	if recErr := recordNodeIdentity(cfg.DataDir, res.FQDN, res.TailscaleIP); recErr != nil {
		return res, fmt.Errorf("save node address: %w", recErr)
	}
	return res, nil
}

// libraryEnrolled reports whether the library has joined a tailnet: the
// node has saved state and an enrolment recorded the address the tailnet
// gave it. A rejected auth key also leaves node state behind (an
// unregistered node key), so the state file alone does not mean enrolled.
func libraryEnrolled(dataDir string) bool {
	if !tsnetnode.Enrolled(tsnetnode.StateDir(dataDir)) {
		return false
	}
	cfg, err := config.Load(dataDir)
	return err == nil && (cfg.TailscaleIP != "" || cfg.PublicURL != "")
}

// redactKey removes the auth key from an error message before it is shown.
func redactKey(err error, key string) error {
	if err == nil || strings.TrimSpace(key) == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "[auth key]"))
}

// serveArgs is the serve invocation a service runs for this data dir: over
// the tailnet once the node is enrolled, otherwise on the configured
// loopback address. An unenrolled `serve --tsnet` would wait for an
// interactive login nobody can complete, so it is never installed.
func serveArgs(dataDir, goos string) []string {
	args := []string{"serve"}
	if libraryEnrolled(dataDir) {
		args = append(args, "--tsnet")
	}
	args = append(args, "--data-dir", dataDir)
	if goos == "windows" {
		// Task Scheduler does not capture output; the server writes its
		// own log and releases the console window it was started with.
		args = append(args, "--log-file", serveLogPath(dataDir))
	}
	return args
}

func serveLogPath(dataDir string) string { return filepath.Join(dataDir, "logs", "serve.log") }

// newServiceManager is replaced in tests.
var newServiceManager = service.New

// executablePath is replaced in tests.
var executablePath = os.Executable

func serviceSpec(m service.Manager, name, dataDir string) (service.Spec, error) {
	exe, err := executablePath()
	if err != nil {
		return service.Spec{}, err
	}
	exe, err = service.ResolveExecutable(exe, os.Getenv)
	if err != nil {
		return service.Spec{}, err
	}
	if name == "" {
		name = m.DefaultName()
	}
	spec := service.Spec{
		Name:       name,
		Executable: exe,
		Args:       serveArgs(dataDir, runtime.GOOS),
		LogPath:    serveLogPath(dataDir),
		WorkingDir: dataDir,
	}
	if runtime.GOOS == "windows" {
		if u, err := user.Current(); err == nil {
			spec.User = u.Username
		}
	}
	return spec, nil
}

// installService registers (or re-registers) the per-user service that runs
// serve for dataDir and starts it. setup --install-service, the service
// command and the setup wizard all go through here.
func installService(ctx context.Context, name, dataDir string) (service.Status, service.Spec, error) {
	m, err := newServiceManager()
	if err != nil {
		return service.Status{}, service.Spec{}, err
	}
	spec, err := serviceSpec(m, name, dataDir)
	if err != nil {
		return service.Status{}, spec, err
	}
	st, err := m.Install(ctx, spec)
	return st, spec, err
}

func uninstallService(ctx context.Context, name string) (service.Status, error) {
	m, err := newServiceManager()
	if err != nil {
		return service.Status{}, err
	}
	if name == "" {
		name = m.DefaultName()
	}
	return m.Uninstall(ctx, name)
}

func serviceStatus(ctx context.Context, name string) (service.Status, error) {
	m, err := newServiceManager()
	if err != nil {
		return service.Status{}, err
	}
	if name == "" {
		name = m.DefaultName()
	}
	return m.Status(ctx, name)
}

// waitForServer waits until the server for dataDir reports serving, has
// failed, or timeout passes, and returns what it last saw.
func waitForServer(ctx context.Context, dataDir string, timeout time.Duration) runstate.Live {
	deadline := time.Now().Add(timeout)
	for {
		live := runstate.Observe(dataDir)
		if (live.Running && live.State == runstate.StateServing) || time.Now().After(deadline) {
			return live
		}
		select {
		case <-ctx.Done():
			return live
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// actionEnableHTTPS is the admin-console step a tailnet without MagicDNS or
// HTTPS certificates needs before the library can serve.
const actionEnableHTTPS = "Enable MagicDNS and HTTPS Certificates at https://login.tailscale.com/admin/dns"

// statusReporter serializes updates to the status file of the data dir this
// process owns.
type statusReporter struct {
	mu      sync.Mutex
	dataDir string
	st      runstate.Status
}

func newStatusReporter(dataDir string) *statusReporter {
	r := &statusReporter{dataDir: dataDir, st: runstate.Status{
		PID: os.Getpid(), Version: version, State: runstate.StateStarting, StartedAt: time.Now().UTC(),
	}}
	r.update(func(*runstate.Status) {})
	return r
}

func (r *statusReporter) update(fn func(*runstate.Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.st)
	if err := runstate.WriteStatus(r.dataDir, r.st); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", runstate.StatusPath(r.dataDir), err)
	}
}

// selfCheckInterval is how often a tailnet server re-runs its reachability
// checks; certificates renew on their own, this only keeps doctor current.
const selfCheckInterval = 6 * time.Hour

// selfCheckLoop records whether the tailnet can resolve this node and whether
// its HTTPS certificate is valid, right after the listener comes up and then
// every selfCheckInterval. The first HTTPS check also obtains the
// certificate, so the first phone to connect does not wait for it.
func selfCheckLoop(ctx context.Context, node *tsnetnode.Node, status *statusReporter, logf func(string, ...any)) {
	for {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		dnsErr := node.CheckMagicDNS(checkCtx)
		notAfter, httpsErr := node.CheckHTTPS(checkCtx, nil)
		cancel()
		if ctx.Err() != nil {
			return
		}
		now := time.Now().UTC()
		status.update(func(st *runstate.Status) {
			st.MagicDNS = checkResult(dnsErr, "resolves to "+node.TailscaleIP(), now)
			st.HTTPS = checkResult(httpsErr, "certificate valid for "+node.FQDN(), now)
			st.CertNotAfter = nil
			if httpsErr == nil {
				na := notAfter.UTC()
				st.CertNotAfter = &na
			}
		})
		for _, err := range []error{dnsErr, httpsErr} {
			if err != nil {
				logf("self-check: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(selfCheckInterval):
		}
	}
}

func checkResult(err error, okDetail string, at time.Time) *runstate.Check {
	if err != nil {
		return &runstate.Check{OK: false, Detail: err.Error(), CheckedAt: at}
	}
	return &runstate.Check{OK: true, Detail: okDetail, CheckedAt: at}
}

// reportingIndexer calls after each time a full scan succeeds.
type reportingIndexer struct {
	*index.Index
	after func()
}

func (r reportingIndexer) ScanAll(ctx context.Context) error {
	err := r.Index.ScanAll(ctx)
	if err == nil {
		r.after()
	}
	return err
}

func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
