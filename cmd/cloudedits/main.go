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
  setup        Configure photo folders and Tailscale node (--json supported)
  serve        Run the Remote Library HTTP API v1 server (--listen or --tsnet)
  status       Show library config, folders, and index summary (--json)
  doctor       Verify folders, .hdrtoys writability, SQLite WAL, and endpoint health (--json)
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
	hostname := fs.String("hostname", "library", "Tailscale node hostname")
	name := fs.String("name", "", "Library display name")
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "rebuildable index/cache/state directory")
	sidecarDir := fs.String("sidecar-dir", "", "optional external sidecar directory for :ro mounts")
	authKeyStdin := fs.Bool("tailscale-auth-key-stdin", false, "read one-time Tailscale auth key from stdin")
	installService := fs.Bool("install-service", false, "record service install intent")
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
	} else if envKey := strings.TrimSpace(os.Getenv("TS_AUTHKEY")); envKey != "" {
		authKey = envKey
	}

	var fqdn string
	var tailscaleIP string
	var actionRequired string
	if authKey != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		node, err := tsnetnode.Start(ctx, tsnetnode.Options{
			Hostname: cfg.Hostname,
			StateDir: tsnetnode.StateDir(cfg.DataDir),
			AuthKey:  authKey,
		})
		if node != nil {
			fqdn = node.FQDN()
			tailscaleIP = node.TailscaleIP()
			_ = node.Close()
		}
		if errors.Is(err, tsnetnode.ErrHTTPSNotEnabled) {
			actionRequired = "Enable MagicDNS and HTTPS Certificates at https://login.tailscale.com/admin/dns"
			out := map[string]any{
				"ok":             false,
				"exitCode":       exitActionRequired,
				"actionRequired": actionRequired,
				"hostname":       cfg.Hostname,
				"fqdn":           fqdn,
				"tailscaleIp":    tailscaleIP,
				"folders":        cfg.Folders,
			}
			emitOutput(*jsonOut, out, "ACTION REQUIRED: "+actionRequired)
			return exitActionRequired
		}
		if err != nil {
			return failOut(*jsonOut, exitActionRequired, "Tailscale registration failed", err)
		}
	}

	res := map[string]any{
		"ok":             true,
		"libraryId":      cfg.LibraryID,
		"libraryName":    cfg.LibraryName,
		"hostname":       cfg.Hostname,
		"fqdn":           fqdn,
		"tailscaleIp":    tailscaleIP,
		"dataDir":        cfg.DataDir,
		"sidecarDir":     cfg.SidecarDir,
		"folders":        cfg.Folders,
		"installService": *installService,
	}
	if fqdn != "" {
		res["url"] = "https://" + fqdn
	}
	emitOutput(*jsonOut, res, fmt.Sprintf("Configured %d folder(s) in %s (hostname=%s)", len(cfg.Folders), cfg.DataDir, cfg.Hostname))
	return exitOK
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
	noWatch := fs.Bool("no-watch", false, "disable fsnotify directory watcher")
	var allowOrigins stringSliceFlag
	fs.Var(&allowOrigins, "allow-origin", "additional browser origin allowed to call the API, e.g. https://preview.example or https://*.example.com (repeatable; the hdr.toys editor and this server's own URL are always allowed)")
	allowDevNetwork := fs.Bool("allow-dev-apis-on-network", false, "permit a dev_apis test build to listen beyond loopback")
	if err := fs.Parse(args); err != nil {
		return exitUsage
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

	idxPath := filepath.Join(cfg.DataDir, "index.db")
	idx, err := index.Open(idxPath, cfg.SidecarDir, cfg.Folders, sidecar.NewStore())
	if err != nil {
		fmt.Fprintf(os.Stderr, "open index: %v\n", err)
		return exitStorage
	}
	defer func() { _ = idx.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Perform initial scan synchronously for small libraries or immediately start serving.
	if err := idx.ScanAll(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "initial scan warning: %v\n", err)
	}
	if !*noWatch {
		if err := idx.StartWatcher(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "watcher warning: %v\n", err)
		}
	}

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
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			return exitUsage
		}
		node, err := tsnetnode.Start(ctx, tsnetnode.Options{
			Hostname: cfg.Hostname,
			StateDir: tsnetnode.StateDir(cfg.DataDir),
			AuthKey:  strings.TrimSpace(os.Getenv("TS_AUTHKEY")),
		})
		if errors.Is(err, tsnetnode.ErrHTTPSNotEnabled) {
			if node != nil {
				_ = node.Close()
			}
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			return exitActionRequired
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "tsnet start failed: %v\n", err)
			return exitActionRequired
		}
		tsNode = node
		defer func() { _ = tsNode.Close() }()
		authenticator.WhoIs = tsNode.WhoIs
		if pubURL == "" && tsNode.FQDN() != "" {
			pubURL = "https://" + tsNode.FQDN()
		}
		ln, err = tsNode.ListenTLS()
		if err != nil {
			fmt.Fprintf(os.Stderr, "tsnet ListenTLS failed: %v\n", err)
			return exitActionRequired
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
			fmt.Fprintf(os.Stderr, "listen %s: %v\n", addr, err)
			return exitStorage
		}
		if pubURL == "" {
			pubURL = "http://" + ln.Addr().String()
		}
	}
	defer func() { _ = ln.Close() }()
	if err := checkTestBuildExposure(api.HasTestRoutes(), ln.Addr(), *allowDevNetwork); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return exitUsage
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
		fmt.Fprintf(os.Stderr, "init API server: %v\n", err)
		return exitStorage
	}

	httpSrv := newHTTPServer(apiSrv)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	cnt, _ := idx.Count(ctx, index.Filter{})
	if tsNode != nil && tsNode.TailscaleIP() != "" {
		fmt.Fprintf(os.Stderr, "cloudedits %s serving %s (Tailscale IP %s, %d assets, %d folders)\n", version, pubURL, tsNode.TailscaleIP(), cnt, len(cfg.Folders))
	} else {
		fmt.Fprintf(os.Stderr, "cloudedits %s serving %s (%d assets, %d folders)\n", version, pubURL, cnt, len(cfg.Folders))
	}
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "serve error: %v\n", err)
		return exitStorage
	}
	return exitOK
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
	}
	emitOutput(*jsonOut, out, fmt.Sprintf("cloudedits %s | %s | %d folder(s) | %d indexed | sidecars: %s",
		version, cfg.LibraryName, len(cfg.Folders), indexed, sidecarMode))
	return exitOK
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dataDir := fs.String("data-dir", config.DefaultDataDir(), "data directory")
	probeURL := fs.String("url", "", "optional running server URL to probe /capabilities")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	cfg, err := config.Load(*dataDir)
	if err != nil {
		return failOut(*jsonOut, exitStorage, "load config failed", err)
	}

	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	var checks []check
	allOK := true

	if cfg.SidecarDir != "" {
		checks = append(checks, check{
			Name:   "sidecar_mode",
			OK:     true,
			Detail: "external --sidecar-dir active (" + cfg.SidecarDir + "); not colocated inside photo folders",
		})
	} else {
		checks = append(checks, check{
			Name:   "sidecar_mode",
			OK:     true,
			Detail: "inside-folder .hdrtoys/ (100% colocated interop)",
		})
	}

	for _, f := range cfg.Folders {
		info, serr := os.Stat(f.Path)
		if serr != nil || !info.IsDir() {
			allOK = false
			checks = append(checks, check{Name: "folder:" + f.ID, OK: false, Detail: "unreadable directory: " + f.Path})
			continue
		}
		sRoot := sidecar.SidecarRoot(f.Path, f.ID, cfg.SidecarDir)
		tmpProbe := filepath.Join(sRoot, ".tmp", ".doctor-probe")
		if mkErr := os.MkdirAll(filepath.Dir(tmpProbe), 0o755); mkErr != nil {
			allOK = false
			checks = append(checks, check{Name: "folder:" + f.ID, OK: false, Detail: "cannot create sidecar dir: " + mkErr.Error()})
			continue
		}
		if wErr := os.WriteFile(tmpProbe, []byte("ok"), 0o644); wErr != nil {
			allOK = false
			checks = append(checks, check{Name: "folder:" + f.ID, OK: false, Detail: "sidecar dir not writable: " + wErr.Error()})
			continue
		}
		_ = os.Remove(tmpProbe)
		checks = append(checks, check{Name: "folder:" + f.ID, OK: true, Detail: "readable + sidecar writable (" + sRoot + ")"})
	}

	// SQLite WAL check.
	idxPath := filepath.Join(cfg.DataDir, "index.db")
	idx, oerr := index.Open(idxPath, cfg.SidecarDir, cfg.Folders, sidecar.NewStore())
	if oerr != nil {
		allOK = false
		checks = append(checks, check{Name: "sqlite_wal", OK: false, Detail: oerr.Error()})
	} else {
		cnt, _ := idx.Count(context.Background(), index.Filter{})
		_ = idx.Close()
		checks = append(checks, check{Name: "sqlite_wal", OK: true, Detail: fmt.Sprintf("WAL healthy (%d indexed assets)", cnt)})
	}

	// Optional HTTP self-probe.
	if u := strings.TrimRight(strings.TrimSpace(*probeURL), "/"); u != "" {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, perr := client.Get(u + "/capabilities")
		if perr != nil {
			allOK = false
			checks = append(checks, check{Name: "http_probe", OK: false, Detail: perr.Error()})
		} else {
			_ = resp.Body.Close()
			ok := resp.StatusCode == http.StatusOK
			if !ok {
				allOK = false
			}
			checks = append(checks, check{Name: "http_probe", OK: ok, Detail: fmt.Sprintf("GET %s/capabilities -> %d", u, resp.StatusCode)})
		}
	}

	out := map[string]any{
		"ok":     allOK,
		"checks": checks,
	}
	msg := "doctor: all checks passed"
	code := exitOK
	if !allOK {
		msg = "doctor: one or more checks failed"
		code = exitStorage
	}
	emitOutput(*jsonOut, out, msg)
	return code
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
	if err := fs.Parse(args[1:]); err != nil {
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
		if fs.NArg() < 1 {
			return failOut(*jsonOut, exitUsage, "missing folder path", nil)
		}
		fc, err := cfg.AddFolder(fs.Arg(0), *label)
		if err != nil {
			return failOut(*jsonOut, exitStorage, "add folder failed", err)
		}
		if err := config.Save(cfg); err != nil {
			return failOut(*jsonOut, exitStorage, "save config failed", err)
		}
		emitOutput(*jsonOut, map[string]any{"added": fc, "folders": cfg.Folders}, "added folder "+fc.Path)
		return exitOK
	case "remove":
		if fs.NArg() < 1 {
			return failOut(*jsonOut, exitUsage, "missing folder path or id", nil)
		}
		removed := cfg.RemoveFolder(fs.Arg(0))
		if err := config.Save(cfg); err != nil {
			return failOut(*jsonOut, exitStorage, "save config failed", err)
		}
		emitOutput(*jsonOut, map[string]any{"removed": removed, "folders": cfg.Folders}, fmt.Sprintf("removed=%v", removed))
		return exitOK
	default:
		return failOut(*jsonOut, exitUsage, "unknown folders subcommand "+sub, nil)
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
		if cfg != nil && cfg.ListenAddr != "" {
			target = "http://" + cfg.ListenAddr
		} else {
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

type guiState struct {
	mu      sync.RWMutex
	cfg     *config.Config
	pubURL  string
	indexed int
}

func (g *guiState) GetConfig() *config.Config {
	g.mu.RLock()
	defer g.mu.RUnlock()
	cp := *g.cfg
	return &cp
}

func (g *guiState) SaveConfig(cfg *config.Config) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := config.Save(cfg); err != nil {
		return err
	}
	g.cfg = cfg
	return nil
}

func (g *guiState) PublicURL() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.pubURL
}

func (g *guiState) IndexedCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.indexed
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
	listenAddr := cfg.ListenAddr
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8787"
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", *listen, err)
		return exitStorage
	}
	state := &guiState{
		cfg:     cfg,
		pubURL:  "http://" + listenAddr,
		indexed: indexed,
	}
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
