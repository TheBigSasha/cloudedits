// Package setupui serves the embedded loopback setup wizard for configuring
// photo folders, Tailscale auth, MagicDNS/HTTPS checks, QR pairing, and AI
// agent quick-start instructions.
package setupui

import (
	"embed"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/thebigsasha/cloudedits/internal/auth"
	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/qr"
)

const maxRequestBytes = 64 * 1024

//go:embed static/index.html
var staticFS embed.FS

// StateProvider supplies live config and node status to the loopback wizard.
type StateProvider interface {
	GetConfig() *config.Config
	SaveConfig(cfg *config.Config) error
	PublicURL() string
	IndexedCount() int
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
			qrSVG, _ = qr.SVG(pubURL)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"libraryName":  cfg.LibraryName,
			"hostname":     cfg.Hostname,
			"folders":      cfg.Folders,
			"sidecarDir":   cfg.SidecarDir,
			"publicUrl":    pubURL,
			"indexedCount": sp.IndexedCount(),
			"qrSvg":        qrSVG,
		})
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
		writeJSON(w, http.StatusOK, fc)
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
		writeJSON(w, http.StatusOK, map[string]any{
			"hostname":    cfg.Hostname,
			"libraryName": cfg.LibraryName,
		})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
