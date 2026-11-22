// Package setupui serves the embedded loopback setup wizard for configuring
// photo folders, Tailscale auth, MagicDNS/HTTPS checks, QR pairing, and AI
// agent quick-start instructions.
package setupui

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/qr"
)

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
