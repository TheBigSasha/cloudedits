// Package setupui serves the embedded loopback setup wizard: choose photo
// folders, paste a Tailscale auth key, and the wizard joins the tailnet,
// starts the library service and shows the library's link and QR code.
package setupui

import (
	"context"
	"embed"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/thebigsasha/cloudedits/internal/auth"
	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/qr"
)

const maxRequestBytes = 64 * 1024

//go:embed static/index.html
var staticFS embed.FS

// ServerState is what the wizard shows about the library server.
type ServerState struct {
	// Enrolled is true once the node has a tailnet identity.
	Enrolled bool `json:"enrolled"`
	// ServiceInstalled is true when the per-user service is registered.
	ServiceInstalled bool   `json:"serviceInstalled"`
	ServiceManager   string `json:"serviceManager,omitempty"`
	// Running is true while a server process owns the data directory.
	Running bool   `json:"running"`
	State   string `json:"state,omitempty"`
	Error   string `json:"error,omitempty"`
	URL     string `json:"url,omitempty"`
	Indexed *int   `json:"indexed,omitempty"`
	// HTTPS and MagicDNS are the server's own reachability checks; nil
	// until it has run them.
	HTTPS    *Check `json:"https,omitempty"`
	MagicDNS *Check `json:"magicDns,omitempty"`
}

// Check is one server self-check result.
type Check struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// ConnectResult is the outcome of a successful Connect.
type ConnectResult struct {
	// ActionRequired is a step the user must take in the Tailscale admin
	// console before the library can serve (for example enabling HTTPS).
	ActionRequired string
}

// Phase names a step of the connect job.
type Phase string

// Connect job phases.
const (
	PhaseJoining  Phase = "joining"
	PhaseStarting Phase = "starting"
	PhaseDone     Phase = "done"
	PhaseFailed   Phase = "failed"
)

// StateProvider supplies live config and node status to the loopback wizard
// and performs the setup steps.
type StateProvider interface {
	GetConfig() *config.Config
	SaveConfig(cfg *config.Config) error
	PublicURL() string
	IndexedCount() int
	Server(ctx context.Context) ServerState
	// Connect joins the tailnet with authKey (skipped when authKey is empty
	// and the node is already enrolled), then installs and starts the
	// library service. It reports progress through phase.
	Connect(ctx context.Context, authKey string, phase func(Phase)) (ConnectResult, error)
	// ConfigChanged is called after the wizard saved a config change, so a
	// running service can pick it up.
	ConfigChanged(ctx context.Context)
}

// connectTimeout bounds one connect job: joining the tailnet plus starting
// the service.
const connectTimeout = 3 * time.Minute

// maxAuthKeyLen bounds the pasted key; Tailscale keys are under 100 bytes.
const maxAuthKeyLen = 512

type connectJob struct {
	mu             sync.Mutex
	running        bool
	Phase          Phase  `json:"phase,omitempty"`
	Error          string `json:"error,omitempty"`
	ActionRequired string `json:"actionRequired,omitempty"`
}

func (j *connectJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{"running": j.running, "phase": j.Phase, "error": j.Error, "actionRequired": j.ActionRequired}
}

func (j *connectJob) set(fn func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fn()
}

// Handler returns an http.Handler for the loopback setup UI.
func Handler(sp StateProvider) http.Handler {
	return localOnly(newMux(sp))
}

// localOnly admits only requests the wizard's own page makes on this machine.
// The wizard has no login: it reconfigures which folders the library serves,
// so it must be unreachable from the network, from other web pages (which a
// browser happily lets post to http://127.0.0.1), and from DNS-rebound names.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")

		if !auth.IsTrustedLoopbackRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "setup is only available on this machine"})
			return
		}
		// Browsers label every request with its relationship to this origin.
		// Only the wizard's own page (same-origin) and address-bar or link
		// navigations may proceed; another local port counts as "same-site".
		navigation := r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") == "navigate"
		if site := r.Header.Get("Sec-Fetch-Site"); (site == "cross-site" || site == "same-site") && !navigation {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request refused"})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "http" || !strings.EqualFold(u.Host, r.Host) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// A JSON content type cannot be sent cross-origin without a
			// preflight, which this handler never answers.
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func newMux(sp StateProvider) *http.ServeMux {
	mux := http.NewServeMux()
	job := &connectJob{}

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/setup", http.StatusFound)
	})
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile("static/index.html")
		if err != nil {
			http.Error(w, "setup UI unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		cfg := sp.GetConfig()
		pubURL := sp.PublicURL()
		var qrSVG string
		if pubURL != "" {
			// The library's own root redirects to hdr.toys with the
			// library preselected, so the link, the QR code and the
			// open button are all this one URL.
			qrSVG, _ = qr.SVG(pubURL)
		}
		srv := sp.Server(r.Context())
		indexed := sp.IndexedCount()
		if srv.Indexed != nil {
			indexed = *srv.Indexed
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"libraryName":  cfg.LibraryName,
			"hostname":     cfg.Hostname,
			"folders":      cfg.Folders,
			"sidecarDir":   cfg.SidecarDir,
			"publicUrl":    pubURL,
			"indexedCount": indexed,
			"qrSvg":        qrSVG,
			"server":       srv,
			"connect":      job.snapshot(),
		})
	})

	mux.HandleFunc("POST /api/connect", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AuthKey string `json:"authKey"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		key := strings.TrimSpace(req.AuthKey)
		switch {
		case len(key) > maxAuthKeyLen:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "That is not a Tailscale auth key."})
			return
		case key != "" && !strings.HasPrefix(key, "tskey-"):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Tailscale auth keys start with tskey-."})
			return
		case key == "" && !sp.Server(r.Context()).Enrolled:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Paste a Tailscale auth key."})
			return
		case len(sp.GetConfig().Folders) == 0:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Add a photo folder first."})
			return
		}
		busy := false
		job.set(func() {
			if job.running {
				busy = true
				return
			}
			job.running, job.Phase, job.Error, job.ActionRequired = true, PhaseJoining, "", ""
		})
		if busy {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already connecting"})
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
			defer cancel()
			res, err := sp.Connect(ctx, key, func(p Phase) { job.set(func() { job.Phase = p }) })
			job.set(func() {
				job.running = false
				if err != nil {
					job.Phase, job.Error = PhaseFailed, redact(err, key)
					return
				}
				job.Phase, job.ActionRequired = PhaseDone, res.ActionRequired
			})
		}()
		writeJSON(w, http.StatusAccepted, job.snapshot())
	})

	mux.HandleFunc("POST /api/folders", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path  string `json:"path"`
			Label string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		cfg := sp.GetConfig()
		fc, err := cfg.AddFolder(strings.TrimSpace(req.Path), strings.TrimSpace(req.Label))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := sp.SaveConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		sp.ConfigChanged(r.Context())
		writeJSON(w, http.StatusOK, fc)
	})

	mux.HandleFunc("POST /api/folders/remove", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		id := strings.TrimSpace(req.ID)
		path := strings.TrimSpace(req.Path)
		if id == "" && path == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "folder id or path is required"})
			return
		}
		cfg := sp.GetConfig()
		removed := false
		if id != "" {
			removed = cfg.RemoveFolder(id)
		}
		if !removed && path != "" {
			removed = cfg.RemoveFolder(path)
		}
		if removed {
			if err := sp.SaveConfig(cfg); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			sp.ConfigChanged(r.Context())
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"removed": removed,
			"folders": cfg.Folders,
		})
	})

	mux.HandleFunc("POST /api/pick-folder", func(w http.ResponseWriter, r *http.Request) {
		res, err := pickFolder(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("GET /api/browse-folders", func(w http.ResponseWriter, r *http.Request) {
		res, err := browseFolders(r.URL.Query().Get("path"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Hostname    string `json:"hostname"`
			LibraryName string `json:"libraryName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		cfg := sp.GetConfig()
		if h := strings.TrimSpace(req.Hostname); h != "" {
			cfg.Hostname = h
		}
		if n := strings.TrimSpace(req.LibraryName); n != "" {
			cfg.LibraryName = n
		}
		if err := sp.SaveConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		sp.ConfigChanged(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"hostname":    cfg.Hostname,
			"libraryName": cfg.LibraryName,
		})
	})

	return mux
}

// redact returns err's message with the auth key removed, so a key echoed in
// an upstream error never reaches the page.
func redact(err error, key string) string {
	msg := err.Error()
	if key != "" {
		msg = strings.ReplaceAll(msg, key, "[auth key]")
	}
	return msg
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
