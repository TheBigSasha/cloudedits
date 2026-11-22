package sidecar

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Store manages atomic, per-asset mutex-serialized .edits sidecars, edited
// thumbnails, and rendered exports. Source folders are strictly read-only;
// writes happen exclusively under sidecarRoot (default: <folderRoot>/.hdrtoys).
type Store struct {
	mu         sync.Mutex
	assetLocks map[string]*sync.Mutex
	writeCount atomic.Int64
}

// NewStore returns a new Store.
func NewStore() *Store {
	return &Store{
		assetLocks: map[string]*sync.Mutex{},
	}
}

// WriteCount returns the total number of atomic .edits commits performed.
func (s *Store) WriteCount() int64 {
	return s.writeCount.Load()
}

func (s *Store) lockFor(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.assetLocks[key]
	if !ok {
		m = &sync.Mutex{}
		s.assetLocks[key] = m
	}
	return m
}

// SidecarRoot resolves the writable root directory for a photo folder.
// When customSidecarDir is empty (default --sidecar-mode=inside), this returns
// filepath.Join(folderRoot, ".hdrtoys"). When customSidecarDir is set, writes
// go under filepath.Join(customSidecarDir, folderID).
func SidecarRoot(folderRoot, folderID, customSidecarDir string) string {
	if strings.TrimSpace(customSidecarDir) != "" {
		return filepath.Join(customSidecarDir, folderID)
	}
	return filepath.Join(folderRoot, ".hdrtoys")
}

// SafeJoinRel validates a POSIX relative path and joins it under baseDir,
// preventing any path traversal outside baseDir.
func SafeJoinRel(baseDir, relPosix string) (string, error) {
	cleaned := path.Clean("/" + strings.ReplaceAll(relPosix, "\\", "/"))
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("invalid relative path %q", relPosix)
	}
	full := filepath.Join(baseDir, filepath.FromSlash(cleaned))
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	if absFull != absBase && !strings.HasPrefix(absFull, absBase+string(filepath.Separator)) {
		return "", fmt.Errorf("path escape rejected for %q", relPosix)
	}
	return full, nil
}

// CanonicalEditsPath returns <sidecarRoot>/<relPosix>.edits.
func CanonicalEditsPath(sidecarRoot, relPosix string) (string, error) {
	return SafeJoinRel(sidecarRoot, relPosix+".edits")
}

// EditThumbnailPath returns <sidecarRoot>/<relPosix>.edit_thumbnail.jpg.
func EditThumbnailPath(sidecarRoot, relPosix string) (string, error) {
	return SafeJoinRel(sidecarRoot, relPosix+".edit_thumbnail.jpg")
}

// ReadResult describes a sidecar read outcome.
type ReadResult struct {
	Sidecar     *Sidecar
	SourcePath  string
	Quarantined string
}

// Read loads an asset's sidecar following legacy read precedence:
//  1. <sidecarRoot>/<rel>.edits (canonical)
//  2. <folderRoot>/<rel>.edits (colocated legacy)
//  3. <folderRoot>/<rel>.hdrt.json (legacy JSON)
//
// If the canonical .edits file exists on disk but fails parsing, it is moved
// to <sidecarRoot>/.quarantine/ and NOT silently masked by a legacy fallback.
func (s *Store) Read(folderRoot, sidecarRoot, relPosix string) (*ReadResult, error) {
	canonical, err := CanonicalEditsPath(sidecarRoot, relPosix)
	if err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(canonical); err == nil {
		sc, parseErr := Parse(data)
		if parseErr != nil {
			qPath, qErr := quarantineFile(sidecarRoot, relPosix, canonical)
			if qErr != nil {
				return nil, fmt.Errorf("corrupt sidecar %s (quarantine failed: %w): %w", canonical, qErr, parseErr)
			}
			return &ReadResult{Quarantined: qPath}, nil
		}
		return &ReadResult{Sidecar: sc, SourcePath: canonical}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// Legacy read-only fallbacks (never written to).
	colocated, err := SafeJoinRel(folderRoot, relPosix+".edits")
	if err == nil {
		if data, rerr := os.ReadFile(colocated); rerr == nil {
			if sc, perr := Parse(data); perr == nil {
				return &ReadResult{Sidecar: sc, SourcePath: colocated}, nil
			}
		}
	}
	legacyJSON, err := SafeJoinRel(folderRoot, relPosix+".hdrt.json")
	if err == nil {
		if data, rerr := os.ReadFile(legacyJSON); rerr == nil {
			if sc, perr := Parse(data); perr == nil {
				if sc.Document == nil {
					sc.Document = &DocumentMeta{Role: "truth"}
				}
				return &ReadResult{Sidecar: sc, SourcePath: legacyJSON}, nil
			}
		}
	}

	return &ReadResult{}, nil
}

// WriteAtomic writes data atomically using <sidecarRoot>/.tmp/<uuid> -> fsync
// -> close -> rename, creating parent directories as needed.
func WriteAtomic(sidecarRoot, targetPath string, data []byte) error {
	tmpDir := filepath.Join(sidecarRoot, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("create .tmp dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	tmpPath := filepath.Join(tmpDir, randomHex(16)+".tmp")
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write staging file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync staging file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close staging file: %w", err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("atomic rename: %w", err)
	}
	cleanup = false

	// Best-effort directory fsync for crash durability.
	if dir, err := os.Open(filepath.Dir(targetPath)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// Update atomically reads the current disk state for an asset, invokes mutate,
// merges, and commits atomically to <sidecarRoot>/<relPosix>.edits if changed.
func (s *Store) Update(
	folderRoot, sidecarRoot, relPosix string,
	sizeBytes *int64,
	contentHash string,
	mutate func(sc *Sidecar) (changed bool, err error),
) (*Sidecar, bool, error) {
	canonical, err := CanonicalEditsPath(sidecarRoot, relPosix)
	if err != nil {
		return nil, false, err
	}

	lk := s.lockFor(canonical)
	lk.Lock()
	defer lk.Unlock()

	res, err := s.Read(folderRoot, sidecarRoot, relPosix)
	if err != nil {
		return nil, false, err
	}
	sc := res.Sidecar
	if sc == nil {
		sc = Empty(path.Base(relPosix), sizeBytes, contentHash)
	} else {
		if sc.Asset == nil {
			sc.Asset = map[string]any{}
		}
		if _, hasFile := sc.Asset["file"]; !hasFile {
			sc.Asset["file"] = path.Base(relPosix)
		}
		if sizeBytes != nil {
			if _, hasBytes := sc.Asset["bytes"]; !hasBytes {
				sc.Asset["bytes"] = *sizeBytes
			}
		}
		if contentHash != "" {
			if _, hasHash := sc.Asset["hash"]; !hasHash {
				sc.Asset["hash"] = contentHash
			}
		}
	}

	changed, err := mutate(sc)
	if err != nil {
		return nil, false, err
	}
	if !changed {
		return sc, false, nil
	}

	payload, err := Serialize(sc)
	if err != nil {
		return nil, false, err
	}
	if err := WriteAtomic(sidecarRoot, canonical, payload); err != nil {
		return nil, false, err
	}
	s.writeCount.Add(1)
	return sc, true, nil
}

// WriteEditThumbnail atomically writes an edited preview JPEG to
// <sidecarRoot>/<relPosix>.edit_thumbnail.jpg.
func (s *Store) WriteEditThumbnail(sidecarRoot, relPosix string, jpegBytes []byte) (string, error) {
	target, err := EditThumbnailPath(sidecarRoot, relPosix)
	if err != nil {
		return "", err
	}
	lk := s.lockFor(target)
	lk.Lock()
	defer lk.Unlock()

	if err := WriteAtomic(sidecarRoot, target, jpegBytes); err != nil {
		return "", err
	}
	return target, nil
}

// RenderEntry describes a stored rendered export for an asset.
type RenderEntry struct {
	RenderID  string `json:"renderId"`
	VariantID string `json:"variantId,omitempty"`
	FileName  string `json:"fileName"`
	MIME      string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
	UpdatedAt string `json:"updatedAt"`
}

// SaveRender idempotently stores a rendered export under
// <sidecarRoot>/renders/<relPosix>/<renderID>.<ext>.
func (s *Store) SaveRender(sidecarRoot, relPosix, renderID, variantID, mimeType string, data []byte) (*RenderEntry, bool, error) {
	cleanID := sanitizeToken(renderID)
	if cleanID == "" {
		cleanID = randomHex(8)
	}
	ext := ".jpg"
	switch strings.ToLower(mimeType) {
	case "image/jxl":
		ext = ".jxl"
	case "image/png":
		ext = ".png"
	case "image/avif":
		ext = ".avif"
	case "image/webp":
		ext = ".webp"
	}
	relTarget := path.Join("renders", relPosix, cleanID+ext)
	target, err := SafeJoinRel(sidecarRoot, relTarget)
	if err != nil {
		return nil, false, err
	}
	lk := s.lockFor(target)
	lk.Lock()
	defer lk.Unlock()

	if info, err := os.Stat(target); err == nil {
		return &RenderEntry{
			RenderID:  cleanID,
			VariantID: variantID,
			FileName:  filepath.Base(target),
			MIME:      mimeType,
			SizeBytes: info.Size(),
			UpdatedAt: info.ModTime().UTC().Format(time.RFC3339),
		}, false, nil
	}

	if err := WriteAtomic(sidecarRoot, target, data); err != nil {
		return nil, false, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, false, err
	}
	return &RenderEntry{
		RenderID:  cleanID,
		VariantID: variantID,
		FileName:  filepath.Base(target),
		MIME:      mimeType,
		SizeBytes: info.Size(),
		UpdatedAt: info.ModTime().UTC().Format(time.RFC3339),
	}, true, nil
}

// ListRenders returns all stored rendered exports for an asset.
func (s *Store) ListRenders(sidecarRoot, relPosix string) ([]RenderEntry, error) {
	dir, err := SafeJoinRel(sidecarRoot, path.Join("renders", relPosix))
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []RenderEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]RenderEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		mime := "image/jpeg"
		switch ext {
		case ".jxl":
			mime = "image/jxl"
		case ".png":
			mime = "image/png"
		case ".avif":
			mime = "image/avif"
		case ".webp":
			mime = "image/webp"
		}
		out = append(out, RenderEntry{
			RenderID:  strings.TrimSuffix(name, filepath.Ext(name)),
			FileName:  name,
			MIME:      mime,
			SizeBytes: info.Size(),
			UpdatedAt: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RenderID < out[j].RenderID })
	return out, nil
}

func quarantineFile(sidecarRoot, relPosix, srcPath string) (string, error) {
	qRel := path.Join(".quarantine", fmt.Sprintf("%d-%s.edits", time.Now().UTC().UnixNano(), strings.ReplaceAll(relPosix, "/", "_")))
	dstPath, err := SafeJoinRel(sidecarRoot, qRel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(srcPath, dstPath); err != nil {
		return "", err
	}
	return dstPath, nil
}

func sanitizeToken(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
