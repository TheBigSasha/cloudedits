package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/thebigsasha/cloudedits/internal/auth"
	"github.com/thebigsasha/cloudedits/internal/formats"
	"github.com/thebigsasha/cloudedits/internal/handoff"
	"github.com/thebigsasha/cloudedits/internal/hlc"
	"github.com/thebigsasha/cloudedits/internal/index"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
	"github.com/thebigsasha/cloudedits/internal/thumb"
)

const (
	remoteLibraryProfile  = "chrome-local-parity/1"
	maxOpsBatch           = 100
	maxRangeLimit         = 500
	maxCursorLimit        = 200
	maxEditThumbnailBytes = 4 * 1024 * 1024
	maxRenderBytes        = 64 * 1024 * 1024
	maxHandoffBytes       = 64 * 1024
	maxConcurrentStreams  = 16
)

// Options configures a Remote Library HTTP API Server.
type Options struct {
	LibraryID     string
	LibraryName   string
	ScratchName   string
	PublicBaseURL string
	EditorBaseURL string
	Index         *index.Index
	Thumbs        *thumb.Service
	Handoffs      *handoff.Store
	Auth          auth.Authenticator
	Authorizer    auth.Authorizer
	Sessions      *auth.SessionManager
	Clock         *hlc.Clock
}

// Server serves the hdr.toys Remote Library HTTP API v1.
type Server struct {
	opts          Options
	idx           *index.Index
	thumbs        *thumb.Service
	handoffs      *handoff.Store
	auth          auth.Authenticator
	authz         auth.Authorizer
	sessions      *auth.SessionManager
	clock         *hlc.Clock
	v1Clock       *hlc.Clock
	streamSem     chan struct{}
	activeStreams atomic.Int64
	routes        []RouteSpec
	handler       http.Handler
}

// New constructs and validates the HTTP Server, failing closed on any
// misconfigured RouteSpec.
func New(opts Options) (*Server, error) {
	if opts.Index == nil {
		return nil, errors.New("api.New requires Index")
	}
	if opts.LibraryName == "" {
		opts.LibraryName = "Cloud Library"
	}
	if opts.LibraryID == "" {
		opts.LibraryID = "cloudedits"
	}
	if opts.EditorBaseURL == "" {
		opts.EditorBaseURL = "https://hdr.toys"
	}
	if opts.Thumbs == nil {
		opts.Thumbs = thumb.NewService("", 0)
	}
	if opts.Handoffs == nil {
		opts.Handoffs = handoff.NewStore(5 * time.Minute)
	}
	if opts.Sessions == nil {
		opts.Sessions = auth.NewSessionManager(30 * time.Minute)
	}
	if opts.Authorizer == nil {
		opts.Authorizer = auth.DefaultAuthorizer{}
	}
	if opts.Auth == nil {
		opts.Auth = &auth.CompositeAuthenticator{
			Sessions:      opts.Sessions,
			AllowLoopback: true,
		}
	}
	if opts.Clock == nil {
		opts.Clock = hlc.NewClock("cloudedits", nil)
	}

	s := &Server{
		opts:      opts,
		idx:       opts.Index,
		thumbs:    opts.Thumbs,
		handoffs:  opts.Handoffs,
		auth:      opts.Auth,
		authz:     opts.Authorizer,
		sessions:  opts.Sessions,
		clock:     opts.Clock,
		v1Clock:   hlc.NewClock("v1", func() time.Time { return time.UnixMilli(1) }),
		streamSem: make(chan struct{}, maxConcurrentStreams),
	}

	routes := s.coreRoutes()
	routes = append(routes, s.extraRoutes()...)
	if err := ValidateRoutes(routes, extraModeEnabled); err != nil {
		return nil, err
	}
	s.routes = routes

	mux := http.NewServeMux()
	for _, spec := range routes {
		specCopy := spec
		mux.HandleFunc(specCopy.Method+" "+specCopy.Pattern, s.wrapRoute(specCopy))
	}
	mux.HandleFunc("/", s.handleFallback)
	s.handler = s.withHeadersLaw(mux)
	return s, nil
}

func (s *Server) handleFallback(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/__dev/") {
		writeAPIError(w, http.StatusNotFound, "not-found", "not found")
		return
	}
	writeAPIError(w, http.StatusNotImplemented, "unsupported-capability", "unsupported capability")
}

// Routes returns a copy of the registered declarative RouteSpecs.
func (s *Server) Routes() []RouteSpec {
	out := make([]RouteSpec, len(s.routes))
	copy(out, s.routes)
	return out
}

// ServeHTTP dispatches HTTP requests through the Headers Law wrapper and router.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) coreRoutes() []RouteSpec {
	return []RouteSpec{
		{Method: http.MethodGet, Pattern: "/{$}", Auth: auth.AuthPublic, ResourceScope: auth.ScopeNone, SLOBudgetMs: 15, Handler: s.handleRoot},
		{Method: http.MethodGet, Pattern: "/open", Auth: auth.AuthPublic, ResourceScope: auth.ScopeNone, SLOBudgetMs: 15, Handler: s.handleOpenRedirect},
		{Method: http.MethodGet, Pattern: "/h/{id}", Auth: auth.AuthPublic, ResourceScope: auth.ScopeNone, SLOBudgetMs: 15, Handler: s.handleHandoffRedirect},
		{Method: http.MethodGet, Pattern: "/capabilities", Auth: auth.AuthPublic, ResourceScope: auth.ScopeNone, SLOBudgetMs: 15, Handler: s.handleCapabilities},
		{Method: http.MethodPost, Pattern: "/auth/session", Auth: auth.AuthPublic, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 25, Handler: s.handleAuthSession},

		{Method: http.MethodGet, Pattern: "/assets/count", Auth: auth.AuthAuthenticated, Permission: auth.PermLibraryRead, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 30, Handler: s.handleAssetsCount},
		{Method: http.MethodGet, Pattern: "/assets/range", Auth: auth.AuthAuthenticated, Permission: auth.PermLibraryRead, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 60, Handler: s.handleAssetsRange},
		{Method: http.MethodGet, Pattern: "/assets", Auth: auth.AuthAuthenticated, Permission: auth.PermLibraryRead, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 60, Handler: s.handleAssetsList},
		{Method: http.MethodGet, Pattern: "/assets/{id}", Auth: auth.AuthAuthenticated, Permission: auth.PermAssetRead, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 30, Handler: s.handleAssetGet},
		{Method: http.MethodGet, Pattern: "/assets/{id}/blob", Auth: auth.AuthAuthenticated, Permission: auth.PermAssetRead, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 40, Handler: s.handleAssetBlob},
		{Method: http.MethodGet, Pattern: "/assets/{id}/file", Auth: auth.AuthAuthenticated, Permission: auth.PermAssetRead, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 40, Handler: s.handleAssetBlob},
		{Method: http.MethodGet, Pattern: "/assets/{id}/recipe", Auth: auth.AuthAuthenticated, Permission: auth.PermAssetRead, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 30, Handler: s.handleAssetRecipe},
		{Method: http.MethodPost, Pattern: "/assets/{id}/rating", Auth: auth.AuthAuthenticated, Permission: auth.PermEditsWrite, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 50, Handler: s.handlePostRating},
		{Method: http.MethodGet, Pattern: "/assets/{id}/variants", Auth: auth.AuthAuthenticated, Permission: auth.PermAssetRead, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 30, Handler: s.handleAssetVariants},
		{Method: http.MethodGet, Pattern: "/assets/{id}/variants/{variantId}/recipe", Auth: auth.AuthAuthenticated, Permission: auth.PermAssetRead, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 30, Handler: s.handleVariantRecipe},
		{Method: http.MethodDelete, Pattern: "/assets/{id}/variants/{variantId}", Auth: auth.AuthAuthenticated, Permission: auth.PermVariantsWrite, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 50, Handler: s.handleDeleteVariant},
		{Method: http.MethodPut, Pattern: "/assets/{id}/edit-thumbnail", Auth: auth.AuthAuthenticated, Permission: auth.PermMediaWrite, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 80, Handler: s.handlePutEditThumbnail},
		{Method: http.MethodPost, Pattern: "/assets/{id}/renders", Auth: auth.AuthAuthenticated, Permission: auth.PermVariantsWrite, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 150, Handler: s.handlePostRender},
		{Method: http.MethodGet, Pattern: "/assets/{id}/renders", Auth: auth.AuthAuthenticated, Permission: auth.PermAssetRead, ResourceScope: auth.ScopeAsset, SLOBudgetMs: 40, Handler: s.handleListRenders},

		{Method: http.MethodPost, Pattern: "/ops", Auth: auth.AuthAuthenticated, Permission: auth.PermEditsWrite, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 50, Handler: s.handlePostOps},
		{Method: http.MethodGet, Pattern: "/changes", Auth: auth.AuthAuthenticated, Permission: auth.PermLibraryRead, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 50, Handler: s.handleChanges},

		{Method: http.MethodPost, Pattern: "/handoff", Auth: auth.AuthAuthenticated, Permission: auth.PermHandoffCreate, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 25, Handler: s.handleCreateHandoff},
		{Method: http.MethodGet, Pattern: "/handoff/{id}", Auth: auth.AuthAuthenticated, Permission: auth.PermAssetRead, ResourceScope: auth.ScopeLibrary, SLOBudgetMs: 25, Handler: s.handleGetHandoff},
	}
}

func (s *Server) wrapRoute(spec RouteSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.preHandle(w, r) {
			return
		}
		switch spec.Auth {
		case auth.AuthPublic:
			spec.Handler(w, r)
			return
		case auth.AuthAuthenticated:
			p, err := s.auth.Authenticate(r)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			resRef := auth.ResourceRef{
				LibraryID: s.opts.LibraryID,
				AssetID:   r.PathValue("id"),
			}
			if err := s.authz.Authorize(p, spec.Permission, spec.ResourceScope, resRef); err != nil {
				if errors.Is(err, auth.ErrUnauthorized) {
					writeError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
				writeError(w, http.StatusForbidden, "forbidden")
				return
			}
			spec.Handler(w, r)
			return
		case auth.AuthDevOnly:
			if !auth.IsLoopbackRemoteAddr(r.RemoteAddr) {
				writeError(w, http.StatusForbidden, "loopback only")
				return
			}
			spec.Handler(w, r)
			return
		default:
			writeError(w, http.StatusInternalServerError, "misconfigured route auth")
		}
	}
}

func (s *Server) withHeadersLaw(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Headers Law: apply CORP + CORS on EVERY response (including 206/304/4xx/5xx/OPTIONS).
		h := w.Header()
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		origin := r.Header.Get("Origin")
		if origin != "" {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
		} else {
			h.Set("Access-Control-Allow-Origin", "*")
		}
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Range, If-Range, If-None-Match, Idempotency-Key, X-HDRT-Profile, X-HDRToys-Client, X-HDRToys-Image-Id")
		h.Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Range, ETag, X-HDRT-Source-Revision, X-HDRT-Tier-Source, Server-Timing")

		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Max-Age", "600")
			h.Set("Server-Timing", formatServerTiming(start))
			w.WriteHeader(http.StatusNoContent)
			return
		}

		tw := &timingResponseWriter{ResponseWriter: w, start: start}
		if hasInvalidTraversalPath(r) {
			writeAPIError(tw, http.StatusBadRequest, "invalid-path", "invalid request path")
			return
		}
		next.ServeHTTP(tw, r)
	})
}

func hasInvalidTraversalPath(r *http.Request) bool {
	candidates := [3]string{r.URL.Path, r.URL.RawPath, ""}
	if reqURI := r.RequestURI; reqURI != "" {
		if q := strings.IndexByte(reqURI, '?'); q >= 0 {
			reqURI = reqURI[:q]
		}
		candidates[2] = reqURI
	}
	for _, raw := range candidates {
		if raw == "" {
			continue
		}
		if strings.ContainsAny(raw, "\\\x00") {
			return true
		}
		lower := strings.ToLower(raw)
		if strings.Contains(lower, "%5c") || strings.Contains(lower, "%00") {
			return true
		}
		for _, seg := range strings.Split(raw, "/") {
			if seg == ".." {
				return true
			}
			if unesc, err := url.PathUnescape(seg); err == nil {
				if unesc == ".." || strings.ContainsAny(unesc, "\\\x00") {
					return true
				}
				if strings.Contains(unesc, "/") {
					for _, sub := range strings.Split(unesc, "/") {
						if sub == ".." {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

type timingResponseWriter struct {
	http.ResponseWriter
	start       time.Time
	wroteHeader bool
}

func (tw *timingResponseWriter) WriteHeader(code int) {
	if !tw.wroteHeader {
		tw.Header().Set("Server-Timing", formatServerTiming(tw.start))
		tw.wroteHeader = true
	}
	tw.ResponseWriter.WriteHeader(code)
}

func (tw *timingResponseWriter) Write(p []byte) (int, error) {
	if !tw.wroteHeader {
		tw.WriteHeader(http.StatusOK)
	}
	return tw.ResponseWriter.Write(p)
}

func (tw *timingResponseWriter) Flush() {
	if f, ok := tw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func formatServerTiming(start time.Time) string {
	durMs := float64(time.Since(start).Microseconds()) / 1000.0
	return fmt.Sprintf("app;dur=%.2f", durMs)
}

func (s *Server) selfOrigin(r *http.Request) string {
	if s.opts.PublicBaseURL != "" {
		return strings.TrimRight(s.opts.PublicBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:8787"
	}
	return scheme + "://" + host
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html") {
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "cloudedits",
			"library": map[string]string{
				"id":   s.opts.LibraryID,
				"name": s.opts.LibraryName,
			},
			"capabilities": s.selfOrigin(r) + "/capabilities",
		})
		return
	}
	target := strings.TrimRight(s.opts.EditorBaseURL, "/") + "/?gallery_api_url=" +
		url.QueryEscape(s.selfOrigin(r)) + "&gallery_name=" + url.QueryEscape(s.opts.LibraryName)
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleOpenRedirect(w http.ResponseWriter, r *http.Request) {
	q := url.Values{}
	q.Set("gallery_api_url", s.selfOrigin(r))
	q.Set("gallery_name", s.opts.LibraryName)
	if a := strings.TrimSpace(r.URL.Query().Get("a")); a != "" {
		q.Set("image_id", a)
	}
	target := strings.TrimRight(s.opts.EditorBaseURL, "/") + "/?" + q.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleHandoffRedirect(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	q := url.Values{}
	q.Set("gallery_api_url", s.selfOrigin(r))
	q.Set("gallery_name", s.opts.LibraryName)
	if id != "" {
		q.Set("handoff", id)
	}
	target := strings.TrimRight(s.opts.EditorBaseURL, "/") + "/?" + q.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	perms := make([]string, 0, len(auth.OwnerPermissions()))
	for _, p := range auth.OwnerPermissions() {
		perms = append(perms, string(p))
	}

	caps := map[string]any{
		"thumbnails":     "server",
		"editThumbnails": "server",
		"ratings":        true,
		"colorLabels":    true,
		"keywords":       true,
		"variants":       true,
		"ops":            true,
		"changes":        true,
		"changeFeed":     true,
		"rangeQueries":   true,
		"writeBack":      true,
		"originals":      "ranged",
		"handoff":        true,
		"acceptsRenders": true,
		"blobs":          []string{"t0", "t1", "t2", "original"},
		"search":         []string{"q", "rating", "flag", "label", "keyword", "kind", "folder"},
	}
	s.extraCaps(caps)

	lib := map[string]any{
		"id":   s.opts.LibraryID,
		"name": s.opts.LibraryName,
	}
	if s.opts.ScratchName != "" {
		lib["scratch"] = s.opts.ScratchName
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version": 1,
		"protocol": map[string]any{
			"name":      "hdrtoys-lp",
			"version":   2,
			"minCompat": 1,
		},
		"profiles":    []string{remoteLibraryProfile},
		"caps":        caps,
		"permissions": perms,
		"limits": map[string]any{
			"maxOpsBatch":           maxOpsBatch,
			"maxRangeLimit":         maxRangeLimit,
			"maxCursorLimit":        maxCursorLimit,
			"maxEditThumbnailBytes": maxEditThumbnailBytes,
			"maxRenderBytes":        maxRenderBytes,
		},
		"library": lib,
	})
}

func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	p, err := s.auth.Authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	token, exp := s.sessions.Issue(p)
	bt := s.sessions.SignBlobToken(p.Subject, "*", 15*time.Minute)
	perms := make([]string, 0, len(p.Scopes))
	for perm, ok := range p.Scopes {
		if ok {
			perms = append(perms, string(perm))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":       token,
		"blobToken":   bt,
		"subject":     p.Subject,
		"expiresAt":   exp.Format(time.RFC3339),
		"permissions": perms,
	})
}

func parseFilter(r *http.Request) index.Filter {
	q := r.URL.Query()
	f := index.Filter{
		Query:    q.Get("q"),
		Flag:     q.Get("flag"),
		Label:    q.Get("label"),
		Keywords: q["keyword"],
		Kind:     strings.Join(q["kind"], ","),
		Folder:   q.Get("folder"),
		Sort:     q.Get("sort"),
		Dir:      q.Get("dir"),
	}
	if v := strings.TrimSpace(q.Get("rating")); v != "" {
		if strings.EqualFold(v, "unrated") {
			f.RatingUnrated = true
		} else if n, err := strconv.Atoi(v); err == nil {
			f.Rating = &n
		}
	}
	if v := q.Get("rating_gte"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.RatingGte = &n
		}
	}
	return f
}

func (s *Server) populateAssetURLs(r *http.Request, a *index.Asset, includeFullEXIF bool) {
	origin := s.selfOrigin(r)
	esc := url.PathEscape(a.ID)
	a.ThumbnailURL = origin + "/assets/" + esc + "/file?tier=thumb"
	a.RawURL = origin + "/assets/" + esc + "/file?tier=original"
	a.RecipeURL = origin + "/assets/" + esc + "/recipe"
	if !includeFullEXIF {
		a.EXIF = nil
	}
}

func (s *Server) handleAssetsCount(w http.ResponseWriter, r *http.Request) {
	cnt, err := s.idx.Count(r.Context(), parseFilter(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": cnt,
		"count": cnt,
		"exact": true,
	})
}

func (s *Server) handleAssetsRange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	if limit > maxRangeLimit {
		limit = maxRangeLimit
	}
	fields := strings.ToLower(strings.TrimSpace(q.Get("fields")))

	items, total, err := s.idx.Range(r.Context(), parseFilter(r), offset, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if fields == "grid" {
		grid := make([]index.GridAsset, len(items))
		for i := range items {
			s.populateAssetURLs(r, &items[i], false)
			grid[i] = items[i].ToGrid()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"offset": offset,
			"limit":  limit,
			"total":  total,
			"exact":  true,
			"assets": grid,
		})
		return
	}

	for i := range items {
		s.populateAssetURLs(r, &items[i], true)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"offset": offset,
		"limit":  limit,
		"total":  total,
		"exact":  true,
		"assets": items,
	})
}

func (s *Server) handleAssetsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cursor := q.Get("cursor")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	if limit > maxCursorLimit {
		limit = maxCursorLimit
	}
	fields := strings.ToLower(strings.TrimSpace(q.Get("fields")))

	items, nextCursor, total, err := s.idx.ListCursor(r.Context(), parseFilter(r), cursor, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if fields == "grid" {
		grid := make([]index.GridAsset, len(items))
		for i := range items {
			s.populateAssetURLs(r, &items[i], false)
			grid[i] = items[i].ToGrid()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"assets":     grid,
			"nextCursor": nextCursor,
			"total":      total,
		})
		return
	}
	for i := range items {
		// Thin list projection (omit heavy full EXIF; GET /assets/{id} serves full EXIF).
		s.populateAssetURLs(r, &items[i], false)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"assets":     items,
		"nextCursor": nextCursor,
		"total":      total,
	})
}

func (s *Server) handleAssetGet(w http.ResponseWriter, r *http.Request) {
	asset, err := s.idx.GetAsset(r.Context(), r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.populateAssetURLs(r, asset, true)
	writeJSON(w, http.StatusOK, asset)
}

func (s *Server) handleAssetBlob(w http.ResponseWriter, r *http.Request) {
	asset, err := s.idx.GetAsset(r.Context(), r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	tier := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tier")))
	if tier == "" || tier == "original" {
		s.serveOriginal(w, r, asset)
		return
	}

	editThumbPath, _ := sidecar.EditThumbnailPath(asset.SidecarRoot, asset.RelPath)
	res, err := s.thumbs.Resolve(r.Context(), asset.AbsPath, asset.SourceRevision, editThumbPath, tier)
	if errors.Is(err, thumb.ErrOverloaded) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "thumbnail worker pool saturated")
		return
	}
	if err != nil {
		// Fall back cleanly to original stream if thumbnail extraction is unsupported for an exotic format.
		if tier == "t2" || tier == "preview" {
			s.serveOriginal(w, r, asset)
			return
		}
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	w.Header().Set("ETag", res.ETag)
	w.Header().Set("X-HDRT-Source-Revision", asset.SourceRevision)
	w.Header().Set("X-HDRT-Tier-Source", res.TierSource)
	w.Header().Set("Cache-Control", "private, max-age=3600, stale-while-revalidate=86400")

	if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm != "" && inm == res.ETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", res.MIME)
	w.Header().Set("Content-Length", strconv.Itoa(len(res.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Data)
}

func (s *Server) serveOriginal(w http.ResponseWriter, r *http.Request, asset *index.Asset) {
	select {
	case s.streamSem <- struct{}{}:
		defer func() { <-s.streamSem }()
	case <-r.Context().Done():
		return
	case <-time.After(1500 * time.Millisecond):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "stream capacity saturated")
		return
	}

	f, err := os.Open(asset.AbsPath)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "source file missing")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.activeStreams.Add(1)
	defer s.activeStreams.Add(-1)

	etag := fmt.Sprintf(`"%s"`, asset.SourceRevision)
	w.Header().Set("Content-Type", asset.MIME)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Encoding", "identity")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-HDRT-Source-Revision", asset.SourceRevision)
	w.Header().Set("X-HDRT-Tier-Source", "original")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename=%q`, asset.Name))

	rs := s.wrapStream(r.Context(), f)
	http.ServeContent(w, r, asset.Name, info.ModTime().UTC(), rs)
}

func (s *Server) handleAssetRecipe(w http.ResponseWriter, r *http.Request) {
	asset, err := s.idx.GetAsset(r.Context(), r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := s.idx.Sidecars().Read(asset.FolderRoot, asset.SidecarRoot, asset.RelPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if res.Sidecar == nil || res.Sidecar.Recipe == nil || res.Sidecar.Recipe.V == nil {
		writeError(w, http.StatusNotFound, "recipe not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"assetId": asset.ID,
		"recipe":  res.Sidecar.Recipe.V,
		"hlc":     res.Sidecar.Recipe.HLC,
	})
}

func (s *Server) handlePostRating(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	var req struct {
		Rating *int `json:"rating"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Rating == nil {
		writeError(w, http.StatusBadRequest, "invalid rating payload")
		return
	}
	rating := *req.Rating
	if rating < 0 || rating > 5 {
		writeError(w, http.StatusBadRequest, "rating must be between 0 and 5")
		return
	}
	// Legacy v1 non-HLC rating write: use the monotonic v1 clock when no modern
	// HLC has touched this asset's rating yet so real HLC sync clients with
	// slight clock skew remain authoritative; otherwise advance the server clock.
	writeHLC := s.v1Clock.Tick()
	if asset, aerr := s.idx.GetAsset(r.Context(), assetID); aerr == nil {
		if res, rerr := s.idx.Sidecars().Read(asset.FolderRoot, asset.SidecarRoot, asset.RelPath); rerr == nil && res.Sidecar != nil {
			if cell, ok := res.Sidecar.Fields["rating"]; ok && !strings.HasSuffix(cell.HLC, "-v1") {
				_ = s.clock.Observe(cell.HLC)
				writeHLC = s.clock.Tick()
			}
		}
	}
	op := sidecar.TruthOp{
		HLC:      writeHLC,
		Device:   "cloudedits-v1",
		AssetKey: assetID,
		Field:    "rating",
		Value:    rating,
	}
	receipts, err := s.idx.ApplyOpsBatch(r.Context(), []sidecar.TruthOp{op})
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"assetId":  assetID,
		"rating":   rating,
		"durable":  true,
		"receipts": receipts,
	})
}

func (s *Server) handleAssetVariants(w http.ResponseWriter, r *http.Request) {
	asset, err := s.idx.GetAsset(r.Context(), r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := s.idx.Sidecars().Read(asset.FolderRoot, asset.SidecarRoot, asset.RelPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	incTomb := strings.EqualFold(r.URL.Query().Get("includeTombstones"), "true") || r.URL.Query().Get("includeTombstones") == "1"
	origin := s.selfOrigin(r)
	escAsset := url.PathEscape(asset.ID)

	variants := []map[string]any{}
	if res.Sidecar != nil {
		for _, vr := range res.Sidecar.Variants {
			if vr.Tombstone && !incTomb {
				continue
			}
			name := vr.ID
			if vr.Name != nil && strings.TrimSpace(*vr.Name) != "" {
				name = *vr.Name
			}
			m := map[string]any{
				"id":        vr.ID,
				"name":      name,
				"hlc":       vr.HLC,
				"recipeUrl": origin + "/assets/" + escAsset + "/variants/" + url.PathEscape(vr.ID) + "/recipe",
			}
			if vr.Recipe != nil {
				m["recipe"] = vr.Recipe
			}
			if vr.Tombstone {
				m["tombstone"] = true
			}
			for k, v := range vr.Extra {
				m[k] = v
			}
			variants = append(variants, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"assetId":  asset.ID,
		"variants": variants,
	})
}

func (s *Server) handleVariantRecipe(w http.ResponseWriter, r *http.Request) {
	asset, err := s.idx.GetAsset(r.Context(), r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	variantID := strings.TrimSpace(r.PathValue("variantId"))
	res, err := s.idx.Sidecars().Read(asset.FolderRoot, asset.SidecarRoot, asset.RelPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if res.Sidecar != nil {
		for _, vr := range res.Sidecar.Variants {
			if vr.ID == variantID && !vr.Tombstone && vr.Recipe != nil {
				writeJSON(w, http.StatusOK, vr.Recipe)
				return
			}
		}
	}
	writeError(w, http.StatusNotFound, "variant recipe not found")
}

func (s *Server) handleDeleteVariant(w http.ResponseWriter, r *http.Request) {
	assetID := r.PathValue("id")
	variantID := strings.TrimSpace(r.PathValue("variantId"))
	if variantID == "" {
		writeError(w, http.StatusBadRequest, "missing variantId")
		return
	}
	op := sidecar.TruthOp{
		HLC:      s.clock.Tick(),
		Device:   "cloudedits",
		AssetKey: assetID,
		Field:    "variant",
		Value: map[string]any{
			"id":        variantID,
			"tombstone": true,
		},
	}
	receipts, err := s.idx.ApplyOpsBatch(r.Context(), []sidecar.TruthOp{op})
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"durable":  true,
		"receipts": receipts,
	})
}

func (s *Server) handlePostOps(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024*1024)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read ops body")
		return
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		writeError(w, http.StatusBadRequest, "empty ops body")
		return
	}

	var ops []sidecar.TruthOp
	switch trimmed[0] {
	case '[':
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&ops); err != nil {
			writeError(w, http.StatusBadRequest, "invalid ops array JSON: "+err.Error())
			return
		}
	case '{':
		var env map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
			writeError(w, http.StatusBadRequest, "invalid ops envelope JSON: "+err.Error())
			return
		}
		rawOps, ok := env["ops"]
		if !ok {
			writeError(w, http.StatusBadRequest, "ops body must be a JSON array or {\"ops\": [...]}")
			return
		}
		dec := json.NewDecoder(strings.NewReader(string(rawOps)))
		dec.UseNumber()
		if err := dec.Decode(&ops); err != nil {
			writeError(w, http.StatusBadRequest, "invalid ops array: "+err.Error())
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "ops body must be a JSON array or {\"ops\": [...]}")
		return
	}

	if len(ops) > maxOpsBatch {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("batch exceeds max %d ops", maxOpsBatch))
		return
	}
	for _, op := range ops {
		_ = s.clock.Observe(op.HLC)
	}

	receipts, err := s.idx.ApplyOpsBatch(r.Context(), ops)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	applied, duplicates, stale := 0, 0, 0
	for _, rc := range receipts {
		switch rc.Status {
		case "applied":
			applied++
		case "duplicate":
			duplicates++
		case "stale":
			stale++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"durable":    true,
		"applied":    applied,
		"duplicates": duplicates,
		"stale":      stale,
		"receipts":   receipts,
	})
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	since := r.URL.Query().Get("since")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	feed, err := s.idx.ChangesSince(r.Context(), since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range feed.Assets {
		s.populateAssetURLs(r, &feed.Assets[i], false)
	}
	writeJSON(w, http.StatusOK, feed)
}

func (s *Server) handlePutEditThumbnail(w http.ResponseWriter, r *http.Request) {
	asset, err := s.idx.GetAsset(r.Context(), r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxEditThumbnailBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "edit thumbnail exceeds 4 MiB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	if !formats.IsJPEGMagic(data) {
		writeError(w, http.StatusUnsupportedMediaType, "edit thumbnail must be a valid JPEG (FF D8 FF)")
		return
	}

	if _, err := s.idx.Sidecars().WriteEditThumbnail(asset.SidecarRoot, asset.RelPath, data); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"assetId":    asset.ID,
		"tierSource": "edit-thumbnail",
		"sizeBytes":  len(data),
	})
}

func (s *Server) handlePostRender(w http.ResponseWriter, r *http.Request) {
	asset, err := s.idx.GetAsset(r.Context(), r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	renderID := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if renderID == "" {
		renderID = strings.TrimSpace(r.URL.Query().Get("renderId"))
	}
	variantID := strings.TrimSpace(r.URL.Query().Get("variantId"))
	mimeType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = "image/jpeg"
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRenderBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "render exceeds 64 MiB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "read render failed")
		return
	}
	if len(data) == 0 {
		writeError(w, http.StatusBadRequest, "empty render payload")
		return
	}

	entry, created, err := s.idx.Sidecars().SaveRender(asset.SidecarRoot, asset.RelPath, renderID, variantID, mimeType, data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created": created,
		"render":  entry,
	})
}

func (s *Server) handleListRenders(w http.ResponseWriter, r *http.Request) {
	asset, err := s.idx.GetAsset(r.Context(), r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "asset not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	entries, err := s.idx.Sidecars().ListRenders(asset.SidecarRoot, asset.RelPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"assetId": asset.ID,
		"renders": entries,
	})
}

func (s *Server) handleCreateHandoff(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxHandoffBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid handoff body")
		return
	}
	if !json.Valid(body) {
		writeError(w, http.StatusBadRequest, "handoff body must be valid JSON")
		return
	}
	entry := s.handoffs.Create(body)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":        entry.ID,
		"url":       s.selfOrigin(r) + "/h/" + entry.ID,
		"expiresAt": entry.ExpiresAt.Format(time.RFC3339),
	})
}

func (s *Server) handleGetHandoff(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	entry, ok := s.handoffs.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "handoff not found or expired")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(entry.Payload)
}

type ctxReadSeeker struct {
	ctx context.Context
	rs  io.ReadSeeker
}

func (c *ctxReadSeeker) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.rs.Read(p)
}

func (c *ctxReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.rs.Seek(offset, whence)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": msg,
		},
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	code := "internal-error"
	switch status {
	case http.StatusBadRequest:
		code = "bad-request"
	case http.StatusUnauthorized:
		code = "unauthorized"
	case http.StatusForbidden:
		code = "forbidden"
	case http.StatusNotFound:
		code = "not-found"
	case http.StatusRequestEntityTooLarge:
		code = "payload-too-large"
	case http.StatusUnsupportedMediaType:
		code = "unsupported-media-type"
	case http.StatusUnprocessableEntity:
		code = "unprocessable-entity"
	case http.StatusTooManyRequests:
		code = "rate-limited"
	case http.StatusNotImplemented:
		code = "unsupported-capability"
	case http.StatusServiceUnavailable:
		code = "service-unavailable"
	}
	writeAPIError(w, status, code, msg)
}
