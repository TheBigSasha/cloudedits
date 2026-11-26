// Command cloudedits is the single-binary remote photo library server for
// hdr.toys, supporting Desktop Setup GUI, Headless CLI, and Docker/NAS modes.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thebigsasha/cloudedits/internal/api"
	"github.com/thebigsasha/cloudedits/internal/auth"
	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/handoff"
	"github.com/thebigsasha/cloudedits/internal/index"
	"github.com/thebigsasha/cloudedits/internal/qr"
	"github.com/thebigsasha/cloudedits/internal/runstate"
	"github.com/thebigsasha/cloudedits/internal/setupui"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
	"github.com/thebigsasha/cloudedits/internal/thumb"
	"github.com/thebigsasha/cloudedits/internal/tsnetnode"
)

const (
	exitOK             = 0
	exitUsage          = 2
	exitActionRequired = 3
	exitStorage        = 4
)

var version = "dev"

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	if strings.TrimSpace(v) != "" {
		*s = append(*s, v)
	}
	return nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return exitUsage
	}
	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "setup":
		return cmdSetup(rest)
	case "serve":
		return cmdServe(rest)
	case "status":
		return cmdStatus(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "folders":
		return cmdFolders(rest)
	case "qr":
		return cmdQR(rest)
	case "docker-init":
		return cmdDockerInit(rest)
	case "service":
		return cmdService(rest)
	case "gui":
		return cmdGUI(rest)
	case "version", "--version", "-v":
		return cmdVersion(rest)
	case "help", "--help", "-h":
		printUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		printUsage()
		return exitUsage
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `cloudedits — single-binary remote photo library server for hdr.toys

Usage:
  cloudedits <command> [flags]

Commands:
  setup        Configure photo folders, join the tailnet, optionally --install-service (--json)
  serve        Run the Remote Library HTTP API v1 server (--listen or --tsnet)
  service      Keep serve running at login: install | uninstall | status (--json)
  status       Show library config, folders, and index summary (--json)
  doctor       Check folders, sidecars, index, tailnet, server, MagicDNS, HTTPS (exit 0/3/4, --json)
  folders      Manage configured photo folders: list | add <dir> | remove <dir|id>
  qr           Print terminal UTF-8 QR code for library or asset (--asset <id>)
  docker-init  Generate docker-compose.yml and .env.example for NAS/Docker
  gui          Launch loopback browser setup wizard (--no-browser supported)
  version      Print version (--json supported)`)
}

type versionInfo struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func cmdVersion(args []string) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	info := versionInfo{
		Version: version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
	emitOutput(*jsonOut, info, fmt.Sprintf("cloudedits %s", version))
	return exitOK
}

func cmdSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	var folders stringSliceFlag
	fs.Var(&folders, "folder", "photo folder path (repeatable)")
	hostname := fs.String("hostname", "", "Tailscale node hostname (default: keep the configured one, initially \"library\")")
	name := fs.String("name", "", "Library display name")
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "rebuildable index/cache/state directory")
	sidecarDir := fs.String("sidecar-dir", "", "optional external sidecar directory for :ro mounts")
	authKeyStdin := fs.Bool("tailscale-auth-key-stdin", false, "read one-time Tailscale auth key from stdin")
	installSvc := fs.Bool("install-service", false, "install and start a per-user service (launchd, systemd --user or a Task Scheduler logon task) that runs serve with this config")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, err := config.Load(*dataDir)
	if err != nil {
		return failOut(*jsonOut, exitStorage, "load config failed", err)
	}
	if strings.TrimSpace(*hostname) != "" {
		cfg.Hostname = strings.TrimSpace(*hostname)
	}
	if strings.TrimSpace(*name) != "" {
		cfg.LibraryName = strings.TrimSpace(*name)
	}
	if strings.TrimSpace(*sidecarDir) != "" {
		cfg.SidecarDir = strings.TrimSpace(*sidecarDir)
	}
	for _, f := range folders {
		if _, err := cfg.AddFolder(f, ""); err != nil {
			return failOut(*jsonOut, exitStorage, "invalid folder "+f, err)
		}
	}
	if err := config.Save(cfg); err != nil {
		return failOut(*jsonOut, exitStorage, "save config failed", err)
	}

	var authKey string
	if *authKeyStdin {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		authKey = strings.TrimSpace(line)
		if authKey == "" {
			return failOut(*jsonOut, exitUsage, "no auth key on stdin", nil)
		}
	} else if envKey := strings.TrimSpace(os.Getenv("TS_AUTHKEY")); envKey != "" {
		authKey = envKey
	}

	res := map[string]any{
		"ok":          true,
		"libraryId":   cfg.LibraryID,
		"libraryName": cfg.LibraryName,
		"hostname":    cfg.Hostname,
		"dataDir":     cfg.DataDir,
		"sidecarDir":  cfg.SidecarDir,
		"folders":     cfg.Folders,
	}
	code := exitOK
	var human []string
	human = append(human, fmt.Sprintf("Configured %d folder(s) in %s (hostname=%s)", len(cfg.Folders), cfg.DataDir, cfg.Hostname))

	if authKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		enrolled, err := enrollNode(ctx, cfg, authKey)
		cancel()
		if err != nil {
			return failOut(*jsonOut, exitActionRequired, "Tailscale registration failed", err)
		}
		res["fqdn"], res["tailscaleIp"] = enrolled.FQDN, enrolled.TailscaleIP
		if enrolled.FQDN != "" {
			res["url"] = "https://" + enrolled.FQDN
			human = append(human, "Joined the tailnet as https://"+enrolled.FQDN)
		}
		if !enrolled.HTTPSEnabled {
			res["ok"] = false
			res["actionRequired"] = actionEnableHTTPS
			code = exitActionRequired
			human = append(human, "ACTION REQUIRED: "+actionEnableHTTPS)
		}
	}
	res["enrolled"] = libraryEnrolled(cfg.DataDir)
	res["serveCommand"] = append([]string{"cloudedits"}, serveArgs(cfg.DataDir, runtime.GOOS)...)

	res["serviceInstalled"] = false
	if *installSvc {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		st, spec, err := installService(ctx, "", cfg.DataDir)
		if err != nil {
			cancel()
			res["ok"] = false
			res["serviceError"] = err.Error()
			emitOutput(*jsonOut, res, strings.Join(append(human,
				"ERROR: could not install the service: "+err.Error(),
				"Run the server yourself with: "+strings.Join(res["serveCommand"].([]string), " ")), "\n"))
			return exitActionRequired
		}
		live := waitForServer(ctx, cfg.DataDir, 30*time.Second)
		cancel()
		res["serviceInstalled"] = st.Installed
		res["service"] = st
		res["serviceArgs"] = spec.Args
		res["server"] = live
		human = append(human, fmt.Sprintf("Installed %s service %s (%s)", st.Manager, st.Name, st.UnitPath))
		switch {
		case live.Running && live.State == runstate.StateServing:
			human = append(human, "Serving "+live.URL)
		case live.Error != "":
			human = append(human, "The server reported: "+live.Error)
			if code == exitOK {
				code = exitActionRequired
				res["ok"] = false
			}
		default:
			human = append(human, "The service is starting; check it with: cloudedits doctor")
		}
	} else {
		human = append(human, "Start the server with: "+strings.Join(res["serveCommand"].([]string), " "),
			"or keep it running with: cloudedits service install")
	}
	emitOutput(*jsonOut, res, strings.Join(human, "\n"))
	return code
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var folders stringSliceFlag
	fs.Var(&folders, "folder", "photo folder path (repeatable; overrides or supplements config)")
	listen := fs.String("listen", "", "HTTP listen address (default 127.0.0.1:8787 when --tsnet is false)")
	useTsnet := fs.Bool("tsnet", false, "serve HTTPS :443 over embedded tailscale.com/tsnet")
	hostname := fs.String("hostname", "", "Tailscale hostname override")
	name := fs.String("name", "", "library name override")
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "rebuildable index/cache/state directory")
	sidecarDir := fs.String("sidecar-dir", "", "optional external sidecar directory (keeps source folders strictly read-only)")
	scratchName := fs.String("scratch-name", "", "optional scratch library label advertised in /capabilities")
	requireToken := fs.Bool("require-token", false, "require Authorization: Bearer token on authenticated endpoints")
	token := fs.String("token", strings.TrimSpace(os.Getenv("CLOUDEDITS_TOKEN")), "static bearer token (prefer the CLOUDEDITS_TOKEN environment variable: flags are visible to other local users)")
	publicURL := fs.String("public-url", "", "override public base URL for redirects and handoff links")
	noWatch := fs.Bool("no-watch", false, "do not follow folder changes after the initial scan")
	var allowOrigins stringSliceFlag
	fs.Var(&allowOrigins, "allow-origin", "additional browser origin allowed to call the API, e.g. https://preview.example or https://*.example.com (repeatable; the hdr.toys editor and this server's own URL are always allowed)")
	allowDevNetwork := fs.Bool("allow-dev-apis-on-network", false, "permit a dev_apis test build to listen beyond loopback")
	logFile := fs.String("log-file", "", "append server output to this file instead of the terminal")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *logFile != "" {
		f, err := openLogFile(*logFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open log file: %v\n", err)
			return exitStorage
		}
		defer func() { _ = f.Close() }()
		os.Stdout, os.Stderr = f, f
		releaseOwnConsole()
	}

	// Enforce a 192 MiB soft memory limit by default so thumbnail bursts stay
	// well under the 256 MiB Peak RSS SLO (and safe in 512 MiB Docker limits)
	// unless the operator explicitly overrides GOMEMLIMIT.
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) == "" {
		debug.SetMemoryLimit(192 << 20)
	}

	cfg, err := config.Load(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return exitStorage
	}
	if strings.TrimSpace(*hostname) != "" {
		cfg.Hostname = strings.TrimSpace(*hostname)
	}
	if strings.TrimSpace(*name) != "" {
		cfg.LibraryName = strings.TrimSpace(*name)
	}
	if strings.TrimSpace(*sidecarDir) != "" {
		cfg.SidecarDir = strings.TrimSpace(*sidecarDir)
	}
	for _, f := range folders {
		if _, err := cfg.AddFolder(f, ""); err != nil {
			fmt.Fprintf(os.Stderr, "folder %q: %v\n", f, err)
			return exitStorage
		}
	}

	// One process per data dir: it owns the tailnet identity and the index.
	lock, err := runstate.Acquire(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return exitActionRequired
	}
	defer func() { _ = lock.Release() }()
	status := newStatusReporter(cfg.DataDir)
	fail := func(code int, format string, args ...any) int {
		msg := fmt.Sprintf(format, args...)
		fmt.Fprintln(os.Stderr, msg)
		status.update(func(st *runstate.Status) {
			st.State, st.Error, st.ExitCode = runstate.StateFailed, strings.TrimPrefix(msg, "ERROR: "), code
		})
		return code
	}

	idxPath := filepath.Join(cfg.DataDir, "index.db")
	idx, err := index.Open(idxPath, cfg.SidecarDir, cfg.Folders, sidecar.NewStore())
	if err != nil {
		return fail(exitStorage, "open index: %v", err)
	}
	defer func() { _ = idx.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sessions := auth.NewSessionManager(30 * time.Minute)
	staticToken, mustUseToken := resolveTokenAuth(cfg, *token, *requireToken)
	authenticator := &auth.CompositeAuthenticator{
		Sessions:      sessions,
		StaticToken:   staticToken,
		RequireToken:  mustUseToken,
		AllowLoopback: !*useTsnet,
	}

	var (
		ln     net.Listener
		tsNode *tsnetnode.Node
		pubURL = strings.TrimSpace(*publicURL)
	)

	if *useTsnet {
		// Fail before joining the tailnet rather than after.
		if err := checkTestBuildExposure(api.HasTestRoutes(), nil, *allowDevNetwork); err != nil {
			return fail(exitUsage, "ERROR: %v", err)
		}
		node, err := tsnetnode.Start(ctx, tsnetOptions(cfg, strings.TrimSpace(os.Getenv("TS_AUTHKEY"))))
		if errors.Is(err, tsnetnode.ErrHTTPSNotEnabled) {
			if node != nil {
				status.update(func(st *runstate.Status) { st.FQDN, st.TailscaleIP = node.FQDN(), node.TailscaleIP() })
				_ = node.Close()
			}
			return fail(exitActionRequired, "ERROR: %s", actionEnableHTTPS)
		}
		if err != nil {
			return fail(exitActionRequired, "tsnet start failed: %v", err)
		}
		tsNode = node
		defer func() { _ = tsNode.Close() }()
		authenticator.WhoIs = tsNode.WhoIs
		if recErr := recordNodeIdentity(cfg.DataDir, tsNode.FQDN(), tsNode.TailscaleIP()); recErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record node address: %v\n", recErr)
		}
		if pubURL == "" && tsNode.FQDN() != "" {
			pubURL = "https://" + tsNode.FQDN()
		}
		ln, err = tsNode.ListenTLS()
		if err != nil {
			return fail(exitActionRequired, "tsnet ListenTLS failed: %v", err)
		}
	} else {
		addr := strings.TrimSpace(*listen)
		if addr == "" {
			addr = cfg.ListenAddr
		}
		if addr == "" {
			addr = "127.0.0.1:8787"
		}
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fail(exitStorage, "listen %s: %v", addr, err)
		}
		if pubURL == "" {
			pubURL = "http://" + ln.Addr().String()
		}
	}
	defer func() { _ = ln.Close() }()
	if err := checkTestBuildExposure(api.HasTestRoutes(), ln.Addr(), *allowDevNetwork); err != nil {
		return fail(exitUsage, "ERROR: %v", err)
	}

	apiSrv, err := api.New(api.Options{
		LibraryID:      cfg.LibraryID,
		LibraryName:    cfg.LibraryName,
		ScratchName:    *scratchName,
		PublicBaseURL:  pubURL,
		AllowedOrigins: append(append([]string{}, cfg.AllowedOrigins...), allowOrigins...),
		Index:          idx,
		Thumbs:         thumb.NewService(filepath.Join(cfg.DataDir, "thumbs"), 0),
		Handoffs:       handoff.NewStore(5 * time.Minute),
		Auth:           authenticator,
		Sessions:       sessions,
	})
	if err != nil {
		return fail(exitStorage, "init API server: %v", err)
	}

	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	cnt, _ := idx.Count(ctx, index.Filter{})
	status.update(func(st *runstate.Status) {
		st.State, st.URL, st.ListenAddr, st.IndexedAssets = runstate.StateServing, pubURL, ln.Addr().String(), cnt
		st.Mode = "listen"
		if tsNode != nil {
			st.Mode, st.FQDN, st.TailscaleIP = "tsnet", tsNode.FQDN(), tsNode.TailscaleIP()
		}
	})
	if tsNode != nil {
		go selfCheckLoop(ctx, tsNode, status, logf)
	}
	if tsNode != nil && tsNode.TailscaleIP() != "" {
		logf("cloudedits %s serving %s (Tailscale IP %s, %d assets already indexed, %d folders)", version, pubURL, tsNode.TailscaleIP(), cnt, len(cfg.Folders))
	} else {
		logf("cloudedits %s serving %s (%d assets already indexed, %d folders)", version, pubURL, cnt, len(cfg.Folders))
	}
	scanned := reportingIndexer{Index: idx, after: func() {
		n, err := idx.Count(context.Background(), index.Filter{})
		if err == nil {
			status.update(func(st *runstate.Status) { st.IndexedAssets = n })
		}
	}}
	if err := serveUntilDone(ctx, ln, apiSrv, scanned, !*noWatch, logf); err != nil {
		return fail(exitStorage, "serve error: %v", err)
	}
	status.update(func(st *runstate.Status) { st.State = runstate.StateStopped })
	return exitOK
}

// backgroundIndexer is the part of the index that serve keeps current while
// the HTTP API is already answering.
type backgroundIndexer interface {
	ScanAll(ctx context.Context) error
	// Watch blocks until ctx is cancelled and no scan is running.
	Watch(ctx context.Context) error
}

// shutdownGrace bounds how long in-flight requests may run after a stop signal.
const shutdownGrace = 5 * time.Second

// serveUntilDone serves handler on ln at once and brings the index up to date
// in the background, so the size of the library never delays the first
// response: requests are answered from the persisted index while the rescan
// runs. It returns only after ctx is cancelled (or the listener failed),
// in-flight requests have drained within shutdownGrace, and the background
// scan has stopped. The caller may close the index as soon as it returns.
func serveUntilDone(ctx context.Context, ln net.Listener, handler http.Handler, idx backgroundIndexer, watch bool, logf func(string, ...any)) error {
	srv := newHTTPServer(handler)

	bgCtx, stopBackground := context.WithCancel(ctx)
	indexed := make(chan struct{})
	go func() {
		defer close(indexed)
		start := time.Now()
		if err := idx.ScanAll(bgCtx); err != nil {
			if bgCtx.Err() != nil {
				return
			}
			logf("initial scan warning: %v", err)
		} else {
			logf("initial scan finished in %s", time.Since(start).Round(time.Millisecond))
		}
		if watch {
			if err := idx.Watch(bgCtx); err != nil {
				logf("watcher warning: %v", err)
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	var err error
	select {
	case err = <-serveErr:
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		if shutErr := srv.Shutdown(shutCtx); shutErr != nil {
			// Requests still running after the grace period (for example a
			// long original download) are cut off rather than left to race
			// the index close.
			_ = srv.Close()
		}
		cancel()
		err = <-serveErr
	}
	stopBackground()
	<-indexed
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "data directory")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, err := config.Load(*dataDir)
	if err != nil {
		return failOut(*jsonOut, exitStorage, "load config failed", err)
	}
	indexed := 0
	idxPath := filepath.Join(cfg.DataDir, "index.db")
	if _, statErr := os.Stat(idxPath); statErr == nil {
		if idx, oerr := index.Open(idxPath, cfg.SidecarDir, cfg.Folders, sidecar.NewStore()); oerr == nil {
			indexed, _ = idx.Count(context.Background(), index.Filter{})
			_ = idx.Close()
		}
	}
	sidecarMode := "inside (.hdrtoys/)"
	if cfg.SidecarDir != "" {
		sidecarMode = "external (" + cfg.SidecarDir + ")"
	}
	out := map[string]any{
		"version":      version,
		"libraryId":    cfg.LibraryID,
		"libraryName":  cfg.LibraryName,
		"hostname":     cfg.Hostname,
		"dataDir":      cfg.DataDir,
		"sidecarMode":  sidecarMode,
		"sidecarDir":   cfg.SidecarDir,
		"folders":      cfg.Folders,
		"indexedCount": indexed,
		"url":          cfg.PublicURL,
		"tailscaleIp":  cfg.TailscaleIP,
	}
	emitOutput(*jsonOut, out, fmt.Sprintf("cloudedits %s | %s | %d folder(s) | %d indexed | sidecars: %s",
		version, cfg.LibraryName, len(cfg.Folders), indexed, sidecarMode))
	return exitOK
}

// doctorCheck is one doctor finding. A failed check carries the exit code
// class it belongs to: exitStorage for local files the library cannot read
// or write, exitActionRequired for a step the user has to take.
type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Status string `json:"status"` // ok | fail | warn | skip
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	exit   int
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "data directory")
	probeURL := fs.String("url", "", "optional running server URL to probe /capabilities")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	checks := runDoctor(context.Background(), *dataDir, strings.TrimRight(strings.TrimSpace(*probeURL), "/"))
	code := exitOK
	for _, c := range checks {
		if c.Status == "fail" && c.exit > code {
			code = c.exit
		}
	}
	var human strings.Builder
	for _, c := range checks {
		fmt.Fprintf(&human, "%-5s %-14s %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
		if c.Fix != "" && c.Status != "ok" {
			fmt.Fprintf(&human, "      %-14s fix: %s\n", "", c.Fix)
		}
	}
	switch code {
	case exitOK:
		human.WriteString("doctor: all checks passed")
	case exitActionRequired:
		human.WriteString("doctor: action required (exit 3)")
	default:
		human.WriteString("doctor: storage problem (exit 4)")
	}
	emitOutput(*jsonOut, map[string]any{"ok": code == exitOK, "exitCode": code, "checks": checks}, human.String())
	return code
}

func runDoctor(ctx context.Context, dataDir, probeURL string) []doctorCheck {
	var checks []doctorCheck
	add := func(c doctorCheck) {
		c.OK = c.Status == "ok"
		checks = append(checks, c)
	}
	cfg, err := config.Load(dataDir)
	if err != nil {
		add(doctorCheck{Name: "config", Status: "fail", Detail: err.Error(), Fix: "repair or remove " + config.ConfigPath(dataDir), exit: exitStorage})
		return checks
	}
	add(doctorCheck{Name: "config", Status: "ok", Detail: config.ConfigPath(cfg.DataDir)})

	if len(cfg.Folders) == 0 {
		add(doctorCheck{Name: "folders", Status: "fail", Detail: "no photo folders configured", Fix: "cloudedits folders add <dir>", exit: exitActionRequired})
	}
	for _, f := range cfg.Folders {
		name := "folder:" + f.ID
		if _, err := os.ReadDir(f.Path); err != nil {
			c := doctorCheck{Name: name, Status: "fail", Detail: "cannot read " + f.Path + ": " + err.Error(), exit: exitStorage}
			if runtime.GOOS == "darwin" && errors.Is(err, os.ErrPermission) {
				c.Fix = "macOS privacy settings denied access; allow cloudedits under System Settings > Privacy & Security > Files and Folders or Full Disk Access"
			}
			add(c)
			continue
		}
		sRoot := sidecar.SidecarRoot(f.Path, f.ID, cfg.SidecarDir)
		if err := probeWritable(filepath.Join(sRoot, ".tmp")); err != nil {
			add(doctorCheck{Name: name, Status: "fail", Detail: "sidecar directory " + sRoot + " is not writable: " + err.Error(),
				Fix: "give this user write access to " + sRoot + ", or set --sidecar-dir to a writable directory", exit: exitStorage})
			continue
		}
		add(doctorCheck{Name: name, Status: "ok", Detail: "readable; sidecars writable in " + sRoot})
	}

	idx, err := index.Open(filepath.Join(cfg.DataDir, "index.db"), cfg.SidecarDir, cfg.Folders, sidecar.NewStore())
	if err != nil {
		add(doctorCheck{Name: "index", Status: "fail", Detail: err.Error(), Fix: "the index is rebuildable: stop the server and delete " + filepath.Join(cfg.DataDir, "index.db*"), exit: exitStorage})
	} else {
		cnt, cerr := idx.Count(ctx, index.Filter{})
		_ = idx.Close()
		if cerr != nil {
			add(doctorCheck{Name: "index", Status: "fail", Detail: cerr.Error(), exit: exitStorage})
		} else {
			add(doctorCheck{Name: "index", Status: "ok", Detail: fmt.Sprintf("%d assets indexed", cnt)})
		}
	}

	stateDir := tsnetnode.StateDir(cfg.DataDir)
	enrolled := libraryEnrolled(cfg.DataDir)
	if enrolled {
		add(doctorCheck{Name: "tailscale", Status: "ok", Detail: "node identity in " + stateDir})
	} else {
		add(doctorCheck{Name: "tailscale", Status: "fail", Detail: "this library has not joined a tailnet",
			Fix: "create an auth key at https://login.tailscale.com/admin/settings/keys, then: cloudedits setup --tailscale-auth-key-stdin", exit: exitActionRequired})
	}

	live := runstate.Observe(cfg.DataDir)
	switch {
	case live.Running && live.State == runstate.StateServing:
		add(doctorCheck{Name: "server", Status: "ok", Detail: fmt.Sprintf("pid %d serving %s since %s", live.PID, live.URL, live.StartedAt.Format(time.RFC3339))})
	case live.Running:
		add(doctorCheck{Name: "server", Status: "fail", Detail: fmt.Sprintf("pid %d is %s", live.PID, live.State), Fix: "wait a few seconds and run doctor again", exit: exitActionRequired})
	default:
		detail := "no server is running for this data directory"
		if live.Error != "" {
			detail += "; last error: " + live.Error
		}
		add(doctorCheck{Name: "server", Status: "fail", Detail: detail, Fix: "cloudedits service install (or run: cloudedits " + strings.Join(serveArgs(cfg.DataDir, runtime.GOOS), " ") + ")", exit: exitActionRequired})
	}

	if svc, err := serviceStatus(ctx, ""); err == nil {
		if svc.Installed {
			add(doctorCheck{Name: "service", Status: "ok", Detail: fmt.Sprintf("%s %s (%s)", svc.Manager, svc.Name, svc.Detail)})
		} else {
			add(doctorCheck{Name: "service", Status: "warn", Detail: "no " + svc.Manager + " service installed; the library stops when this session ends", Fix: "cloudedits service install"})
		}
	}

	serving := live.Running && live.State == runstate.StateServing
	switch {
	case serving && live.Mode == "listen":
		add(probeCapabilities("listener", "http://"+live.ListenAddr))
	case serving && live.Mode == "tsnet":
		add(doctorCheck{Name: "listener", Status: "ok", Detail: "HTTPS listener on the tailnet at " + live.FQDN + ":443"})
		add(selfCheck("magicdns", live.MagicDNS, "Enable MagicDNS at https://login.tailscale.com/admin/dns"))
		https := selfCheck("https_cert", live.HTTPS, actionEnableHTTPS)
		if live.CertNotAfter != nil && https.Status == "ok" {
			https.Detail += ", expires " + live.CertNotAfter.Format("2006-01-02")
		}
		add(https)
	case enrolled:
		for _, name := range []string{"listener", "magicdns", "https_cert"} {
			add(doctorCheck{Name: name, Status: "skip", Detail: "the server is not running"})
		}
	}

	if probeURL != "" {
		add(probeCapabilities("http_probe", probeURL))
	}
	return checks
}

func selfCheck(name string, c *runstate.Check, fix string) doctorCheck {
	if c == nil {
		return doctorCheck{Name: name, Status: "skip", Detail: "the server has not finished this check yet"}
	}
	if !c.OK {
		return doctorCheck{Name: name, Status: "fail", Detail: c.Detail, Fix: fix, exit: exitActionRequired}
	}
	return doctorCheck{Name: name, Status: "ok", Detail: c.Detail + " (checked " + c.CheckedAt.Format(time.RFC3339) + ")"}
}

func probeCapabilities(name, base string) doctorCheck {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(base + "/capabilities")
	if err != nil {
		return doctorCheck{Name: name, Status: "fail", Detail: err.Error(), exit: exitActionRequired}
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return doctorCheck{Name: name, Status: "fail", Detail: fmt.Sprintf("GET %s/capabilities -> %d", base, resp.StatusCode), exit: exitActionRequired}
	}
	return doctorCheck{Name: name, Status: "ok", Detail: fmt.Sprintf("GET %s/capabilities -> 200", base)}
}

// probeWritable creates and removes a file in dir, creating dir if needed.
func probeWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

func cmdService(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cloudedits service <install|uninstall|status> [--data-dir DIR] [--name NAME] [--json]")
		return exitUsage
	}
	sub := args[0]
	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "data directory the service serves")
	name := fs.String("name", "", "service name (default: toys.hdr.cloudedits on macOS, cloudedits elsewhere)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	switch sub {
	case "install":
		cfg, err := config.Load(*dataDir)
		if err != nil {
			return failOut(*jsonOut, exitStorage, "load config failed", err)
		}
		if err := config.Save(cfg); err != nil {
			return failOut(*jsonOut, exitStorage, "save config failed", err)
		}
		st, spec, err := installService(ctx, *name, cfg.DataDir)
		if err != nil {
			return failOut(*jsonOut, exitActionRequired, "install service failed", err)
		}
		live := waitForServer(ctx, cfg.DataDir, 30*time.Second)
		out := map[string]any{"ok": live.Running && live.State == runstate.StateServing, "service": st, "args": spec.Args, "executable": spec.Executable, "server": live}
		msg := fmt.Sprintf("Installed %s service %s (%s)\nruns: %s %s", st.Manager, st.Name, st.UnitPath, spec.Executable, strings.Join(spec.Args, " "))
		if out["ok"] == true {
			emitOutput(*jsonOut, out, msg+"\nserving "+live.URL)
			return exitOK
		}
		reason := "not serving yet; check it with: cloudedits doctor"
		if live.Error != "" {
			reason = "the server reported: " + live.Error
		}
		out["actionRequired"] = reason
		emitOutput(*jsonOut, out, msg+"\n"+reason)
		return exitActionRequired
	case "uninstall":
		st, err := uninstallService(ctx, *name)
		if err != nil {
			return failOut(*jsonOut, exitActionRequired, "uninstall service failed", err)
		}
		emitOutput(*jsonOut, map[string]any{"ok": true, "service": st}, fmt.Sprintf("Removed %s service %s", st.Manager, st.Name))
		return exitOK
	case "status":
		st, err := serviceStatus(ctx, *name)
		if err != nil {
			return failOut(*jsonOut, exitActionRequired, "service status failed", err)
		}
		live := runstate.Observe(*dataDir)
		emitOutput(*jsonOut, map[string]any{"ok": true, "service": st, "server": live},
			fmt.Sprintf("%s %s: installed=%v loaded=%v (%s)\nserver: running=%v state=%s %s", st.Manager, st.Name, st.Installed, st.Loaded, st.Detail, live.Running, live.State, live.URL))
		return exitOK
	default:
		return failOut(*jsonOut, exitUsage, "unknown service subcommand "+sub, nil)
	}
}

func cmdFolders(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: cloudedits folders <list|add|remove> [args]")
		return exitUsage
	}
	sub := args[0]
	fs := flag.NewFlagSet("folders "+sub, flag.ContinueOnError)
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "data directory")
	label := fs.String("label", "", "optional folder label (for add)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	positional, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return exitUsage
	}

	cfg, err := config.Load(*dataDir)
	if err != nil {
		return failOut(*jsonOut, exitStorage, "load config failed", err)
	}

	switch sub {
	case "list":
		emitOutput(*jsonOut, map[string]any{"folders": cfg.Folders}, fmt.Sprintf("%d folder(s) configured", len(cfg.Folders)))
		return exitOK
	case "add":
		if len(positional) < 1 {
			return failOut(*jsonOut, exitUsage, "missing folder path", nil)
		}
		fc, err := cfg.AddFolder(positional[0], *label)
		if err != nil {
			return failOut(*jsonOut, exitStorage, "add folder failed", err)
		}
		if err := config.Save(cfg); err != nil {
			return failOut(*jsonOut, exitStorage, "save config failed", err)
		}
		emitOutput(*jsonOut, map[string]any{"added": fc, "folders": cfg.Folders}, "added folder "+fc.Path)
		return exitOK
	case "remove":
		if len(positional) < 1 {
			return failOut(*jsonOut, exitUsage, "missing folder path or id", nil)
		}
		removed := cfg.RemoveFolder(positional[0])
		if err := config.Save(cfg); err != nil {
			return failOut(*jsonOut, exitStorage, "save config failed", err)
		}
		emitOutput(*jsonOut, map[string]any{"removed": removed, "folders": cfg.Folders}, fmt.Sprintf("removed=%v", removed))
		return exitOK
	default:
		return failOut(*jsonOut, exitUsage, "unknown folders subcommand "+sub, nil)
	}
}

// recordNodeIdentity stores the enrolled node's tailnet address in the config
// on disk. It reloads the stored config rather than saving the caller's
// in-memory copy, so per-invocation overrides (serve --folder, --name, ...)
// are never persisted as a side effect. An empty identity records nothing;
// an address without a MagicDNS name keeps the stored URL.
func recordNodeIdentity(dataDir, fqdn, tailscaleIP string) error {
	if fqdn == "" && tailscaleIP == "" {
		return nil
	}
	stored, err := config.Load(dataDir)
	if err != nil {
		return err
	}
	url := stored.PublicURL
	if fqdn != "" {
		url = "https://" + fqdn
	}
	if stored.PublicURL == url && stored.TailscaleIP == tailscaleIP {
		return nil
	}
	stored.PublicURL, stored.TailscaleIP = url, tailscaleIP
	return config.Save(stored)
}

// parseInterspersed parses flags that may appear before, between or after
// positional arguments (the standard flag package stops at the first
// positional) and returns the positionals in order.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func cmdQR(args []string) int {
	fs := flag.NewFlagSet("qr", flag.ContinueOnError)
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "data directory")
	baseURL := fs.String("url", "", "server base URL (e.g. https://library.tailnet.ts.net)")
	assetID := fs.String("asset", "", "optional asset ID deep link")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, _ := config.Load(*dataDir)
	target := strings.TrimRight(strings.TrimSpace(*baseURL), "/")
	if target == "" {
		switch {
		case cfg != nil && cfg.PublicURL != "":
			target = cfg.PublicURL
		case cfg != nil && cfg.ListenAddr != "":
			target = "http://" + cfg.ListenAddr
		default:
			target = "http://127.0.0.1:8787"
		}
	}
	if a := strings.TrimSpace(*assetID); a != "" {
		target = target + "/open?a=" + url.QueryEscape(a)
	}

	term, err := qr.Terminal(target)
	if err != nil {
		return failOut(*jsonOut, exitStorage, "generate QR failed", err)
	}
	if *jsonOut {
		emitOutput(true, map[string]any{"url": target, "terminal": term}, "")
		return exitOK
	}
	fmt.Println(target)
	fmt.Print(term)
	return exitOK
}

func cmdDockerInit(args []string) int {
	fs := flag.NewFlagSet("docker-init", flag.ContinueOnError)
	folder := fs.String("folder", "/volume1/photos", "host photo directory to mount at /library/photos")
	hostname := fs.String("hostname", "library", "Tailscale node hostname")
	outDir := fs.String("out", "./cloudedits-docker", "output directory for docker-compose.yml")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	composePath, envPath, err := config.WriteDockerBundle(*outDir, *folder, *hostname)
	if err != nil {
		return failOut(*jsonOut, exitStorage, "write docker bundle failed", err)
	}
	emitOutput(*jsonOut, map[string]any{
		"ok":          true,
		"composePath": composePath,
		"envPath":     envPath,
	}, fmt.Sprintf("Wrote %s and %s", composePath, envPath))
	return exitOK
}

// guiState backs the setup wizard with the config on disk. It reloads on
// every read so the page follows changes made by other commands while it is
// open (for example `setup` enrolling the node), and a wizard save starts
// from the stored config instead of overwriting it with a stale copy.
type guiState struct {
	mu      sync.Mutex
	dataDir string
	indexed int
	// serviceName is the service the wizard installs ("" = platform default).
	serviceName string
}

func (g *guiState) GetConfig() *config.Config {
	g.mu.Lock()
	defer g.mu.Unlock()
	cfg, err := config.Load(g.dataDir)
	if err != nil {
		return config.Default(g.dataDir)
	}
	return cfg
}

func (g *guiState) SaveConfig(cfg *config.Config) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return config.Save(cfg)
}

// PublicURL is the library's tailnet URL, or "" until the node is enrolled.
func (g *guiState) PublicURL() string {
	return g.GetConfig().PublicURL
}

func (g *guiState) IndexedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.indexed
}

// Server reports the node, service and server state for the wizard.
func (g *guiState) Server(ctx context.Context) setupui.ServerState {
	out := setupui.ServerState{Enrolled: libraryEnrolled(g.dataDir)}
	if st, err := serviceStatus(ctx, g.serviceName); err == nil {
		out.ServiceInstalled, out.ServiceManager = st.Installed, st.Manager
	}
	live := runstate.Observe(g.dataDir)
	out.Running, out.State, out.Error, out.URL = live.Running, string(live.State), live.Error, live.URL
	if live.Running && live.State == runstate.StateServing {
		n := live.IndexedAssets
		out.Indexed = &n
	}
	if live.HTTPS != nil {
		out.HTTPS = &setupui.Check{OK: live.HTTPS.OK, Detail: live.HTTPS.Detail}
	}
	if live.MagicDNS != nil {
		out.MagicDNS = &setupui.Check{OK: live.MagicDNS.OK, Detail: live.MagicDNS.Detail}
	}
	return out
}

// Connect joins the tailnet with authKey through the same enrolment path as
// `setup`, then installs and starts the library service.
func (g *guiState) Connect(ctx context.Context, authKey string, phase func(setupui.Phase)) (setupui.ConnectResult, error) {
	var res setupui.ConnectResult
	if authKey != "" {
		phase(setupui.PhaseJoining)
		enrolled, err := enrollNode(ctx, g.GetConfig(), authKey)
		if err != nil {
			return res, err
		}
		if !enrolled.HTTPSEnabled {
			res.ActionRequired = actionEnableHTTPS
		}
	}
	phase(setupui.PhaseStarting)
	if _, _, err := installService(ctx, g.serviceName, g.dataDir); err != nil {
		return res, fmt.Errorf("start the library service: %w", err)
	}
	return res, nil
}

// ConfigChanged restarts an installed service so it serves the saved config.
func (g *guiState) ConfigChanged(ctx context.Context) {
	st, err := serviceStatus(ctx, g.serviceName)
	if err != nil || !st.Installed {
		return
	}
	if _, _, err := installService(ctx, g.serviceName, g.dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "restart service after a config change: %v\n", err)
	}
}

func cmdGUI(args []string) int {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:0", "loopback listen address for setup wizard")
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "data directory")
	noBrowser := fs.Bool("no-browser", false, "do not automatically open the default browser")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, err := config.Load(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return exitStorage
	}
	indexed := 0
	idxPath := filepath.Join(cfg.DataDir, "index.db")
	if _, statErr := os.Stat(idxPath); statErr == nil {
		if idx, oerr := index.Open(idxPath, cfg.SidecarDir, cfg.Folders, sidecar.NewStore()); oerr == nil {
			indexed, _ = idx.Count(context.Background(), index.Filter{})
			_ = idx.Close()
		}
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", *listen, err)
		return exitStorage
	}
	state := &guiState{dataDir: cfg.DataDir, indexed: indexed}
	srv := newHTTPServer(setupui.Handler(state))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	setupURL := fmt.Sprintf("http://%s/setup", ln.Addr().String())
	fmt.Printf("Setup wizard listening at %s\n", setupURL)
	if !*noBrowser {
		go func() {
			if err := openBrowser(setupURL); err != nil {
				fmt.Fprintf(os.Stderr, "note: could not open browser automatically (%v); open %s manually\n", err, setupURL)
			}
		}()
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "gui serve error: %v\n", err)
		return exitStorage
	}
	return exitOK
}

func openBrowserCommand(goos, rawURL string, hasDisplay bool) (string, []string, bool) {
	switch goos {
	case "darwin":
		return "open", []string{rawURL}, true
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}, true
	default:
		if !hasDisplay {
			return "", nil, false
		}
		return "xdg-open", []string{rawURL}, true
	}
}

func openBrowser(rawURL string) error {
	hasDisplay := os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	name, args, ok := openBrowserCommand(runtime.GOOS, rawURL, hasDisplay)
	if !ok {
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Env = withoutSecrets(os.Environ())
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// newHTTPServer returns an http.Server with the connection limits every
// cloudedits listener needs. Header reads are bounded against slow-header
// clients and idle keep-alive connections are reaped, so a peer cannot pin
// file descriptors indefinitely. Body reads and response writes stay
// unbounded in time because originals and renders are legitimately large.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

// resolveTokenAuth merges the token settings from config.json with the
// command line: an explicit flag or environment token wins, and token mode is
// required when either the flag or the persisted authMode asks for it.
func resolveTokenAuth(cfg *config.Config, flagToken string, flagRequire bool) (token string, require bool) {
	token = strings.TrimSpace(flagToken)
	if token == "" {
		token = strings.TrimSpace(cfg.BearerToken)
	}
	return token, flagRequire || strings.EqualFold(strings.TrimSpace(cfg.AuthMode), "token")
}

// checkTestBuildExposure refuses to serve a dev_apis test build beyond loopback
// unless the operator opts in. Such builds carry /__dev/reset, which deletes
// the sidecar directory, and must never be reachable from a network by
// accident (for example a test binary started with --tsnet or 0.0.0.0).
func checkTestBuildExposure(testBuild bool, addr net.Addr, allowNetwork bool) error {
	if !testBuild || allowNetwork {
		return nil
	}
	// Only a plain TCP listener bound to a loopback IP is local; a tailnet
	// listener reports a non-TCP or non-loopback address.
	if tcp, ok := addr.(*net.TCPAddr); ok && tcp.IP.IsLoopback() {
		return nil
	}
	where := "the tailnet"
	if addr != nil {
		where = addr.String()
	}
	return fmt.Errorf("this is a dev_apis test build with destructive /__dev/* endpoints; refusing to listen on %s (not loopback). Use a release build, or pass --allow-dev-apis-on-network", where)
}

// secretEnvVars are credentials cloudedits reads from its environment that a
// helper process such as the desktop browser must not inherit.
var secretEnvVars = []string{"TS_AUTHKEY", "TS_AUTH_KEY", "CLOUDEDITS_TOKEN"}

func withoutSecrets(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.ContainsFunc(secretEnvVars, func(s string) bool { return strings.EqualFold(s, name) }) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func emitOutput(asJSON bool, payload any, human string) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(payload)
		return
	}
	if human != "" {
		fmt.Println(human)
	}
}

func failOut(asJSON bool, code int, msg string, err error) int {
	detail := msg
	if err != nil {
		detail = msg + ": " + err.Error()
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"ok":       false,
			"exitCode": code,
			"error":    detail,
		})
	} else {
		_, _ = io.WriteString(os.Stderr, "ERROR: "+detail+"\n")
	}
	return code
}
