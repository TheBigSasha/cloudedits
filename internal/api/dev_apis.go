//go:build dev_apis

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/thebigsasha/cloudedits/internal/auth"
	"github.com/thebigsasha/cloudedits/internal/index"
)

const extraModeEnabled = true

var devFaultStore sync.Map // map[*Server]devFaultConfig

type devFaultConfig struct {
	Offline             bool   `json:"offline"`
	StatusCode          int    `json:"statusCode"`
	ThrottleBytesPerSec int64  `json:"throttleBytesPerSec"`
	AbortAfterBytes     int64  `json:"abortAfterBytes"`
	PathPrefix          string `json:"pathPrefix"`
}

func (s *Server) extraRoutes() []RouteSpec {
	return []RouteSpec{
		{Method: http.MethodPost, Pattern: "/__dev/reset", Auth: auth.AuthDevOnly, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 500, Handler: s.handleDevReset},
		{Method: http.MethodPost, Pattern: "/__dev/fault", Auth: auth.AuthDevOnly, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 50, Handler: s.handleDevFault},
		{Method: http.MethodGet, Pattern: "/__dev/stats", Auth: auth.AuthDevOnly, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 50, Handler: s.handleDevStats},
	}
}

func (s *Server) extraCaps(caps map[string]any) {
	caps["devApis"] = true
}

func (s *Server) getFault() devFaultConfig {
	if v, ok := devFaultStore.Load(s); ok {
		if cfg, ok := v.(devFaultConfig); ok {
			return cfg
		}
	}
	return devFaultConfig{}
}

func (s *Server) setFault(cfg devFaultConfig) {
	devFaultStore.Store(s, cfg)
}

func (s *Server) preHandle(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/__dev/") {
		return false
	}
	cfg := s.getFault()
	if cfg.PathPrefix != "" && !strings.HasPrefix(r.URL.Path, cfg.PathPrefix) {
		return false
	}
	if cfg.Offline {
		writeError(w, http.StatusServiceUnavailable, "simulated offline fault")
		return true
	}
	if cfg.StatusCode >= 400 {
		writeError(w, cfg.StatusCode, "simulated fault")
		return true
	}
	return false
}

func (s *Server) wrapStream(ctx context.Context, rs io.ReadSeeker) io.ReadSeeker {
	cfg := s.getFault()
	base := &ctxReadSeeker{ctx: ctx, rs: rs}
	if cfg.ThrottleBytesPerSec <= 0 && cfg.AbortAfterBytes <= 0 {
		return base
	}
	return &faultReadSeeker{
		ctx:                 ctx,
		rs:                  base,
		throttleBytesPerSec: cfg.ThrottleBytesPerSec,
		abortAfterBytes:     cfg.AbortAfterBytes,
	}
}

type faultReadSeeker struct {
	ctx                 context.Context
	rs                  io.ReadSeeker
	throttleBytesPerSec int64
	abortAfterBytes     int64
	readSoFar           int64
}

func (f *faultReadSeeker) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	if f.abortAfterBytes > 0 && f.readSoFar >= f.abortAfterBytes {
		return 0, errors.New("simulated mid-stream abort")
	}
	if f.abortAfterBytes > 0 {
		rem := f.abortAfterBytes - f.readSoFar
		if int64(len(p)) > rem {
			p = p[:rem]
		}
	}
	n, err := f.rs.Read(p)
	f.readSoFar += int64(n)
	if n > 0 && f.throttleBytesPerSec > 0 {
		delay := time.Duration(int64(time.Second) * int64(n) / f.throttleBytesPerSec)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-f.ctx.Done():
				return n, f.ctx.Err()
			}
		}
	}
	return n, err
}

func (f *faultReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return f.rs.Seek(offset, whence)
}

func (s *Server) handleDevReset(w http.ResponseWriter, r *http.Request) {
	sidecarDir := strings.TrimSpace(s.idx.SidecarDir())
	if sidecarDir == "" {
		writeError(w, http.StatusForbidden, "reset requires an isolated scratch --sidecar-dir outside source folders")
		return
	}
	// Safety check: ensure sidecarDir is not equal to or inside any configured source folder.
	absSidecar, err := filepath.Abs(sidecarDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, f := range s.idx.Folders() {
		absFolder, ferr := filepath.Abs(f.Path)
		if ferr != nil {
			continue
		}
		if absSidecar == absFolder || strings.HasPrefix(absSidecar, absFolder+string(filepath.Separator)) {
			writeError(w, http.StatusForbidden, "refusing to reset sidecar-dir located inside source folder")
			return
		}
	}

	if err := os.RemoveAll(absSidecar); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.MkdirAll(absSidecar, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.thumbs.ClearCache()
	s.handoffs.Clear()
	s.setFault(devFaultConfig{})

	if err := s.idx.ResetIndex(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cnt, _ := s.idx.Count(r.Context(), index.Filter{})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"indexedAssets": cnt,
	})
}

func (s *Server) handleDevFault(w http.ResponseWriter, r *http.Request) {
	var cfg devFaultConfig
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid fault JSON")
			return
		}
	}
	s.setFault(cfg)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"fault": cfg,
	})
}

func (s *Server) handleDevStats(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	openFDs := countOpenFDs()
	indexed, _ := s.idx.Count(r.Context(), index.Filter{})

	writeJSON(w, http.StatusOK, map[string]any{
		"openFds":         openFDs,
		"activeStreams":   s.activeStreams.Load(),
		"heapAllocBytes":  ms.HeapAlloc,
		"heapInuseBytes":  ms.HeapInuse,
		"sysBytes":        ms.Sys,
		"goroutines":      runtime.NumGoroutine(),
		"walBytes":        s.idx.WALSizeBytes(),
		"editsWriteCount": s.idx.Sidecars().WriteCount(),
		"indexedAssets":   indexed,
	})
}

func countOpenFDs() int {
	for _, p := range []string{"/dev/fd", "/proc/self/fd"} {
		if entries, err := os.ReadDir(p); err == nil {
			return len(entries)
		}
	}
	return -1
}
