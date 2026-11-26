package sidecar

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type assetLock struct {
	mu  sync.Mutex
	gen uint64
}

// Store manages atomic, per-asset mutex-serialized .edits sidecars, edited
// thumbnails, and rendered exports. Source folders are strictly read-only;
// writes happen exclusively under sidecarRoot (default: <folderRoot>/.hdrtoys).
type Store struct {
	mu         sync.Mutex
	assetLocks map[string]*assetLock
	writeCount atomic.Int64
}

// NewStore returns a new Store.
func NewStore() *Store {
	return &Store{
		assetLocks: map[string]*assetLock{},
	}
}

// WriteCount returns the total number of atomic .edits commits performed.
func (s *Store) WriteCount() int64 {
	return s.writeCount.Load()
}

func (s *Store) lockFor(key string) *assetLock {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.assetLocks[key]
	if !ok {
		m = &assetLock{}
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
//
// A backslash separates path components only where the host filesystem says
// so (Windows). Elsewhere it is an ordinary filename character: rewriting it
// to "/" would map the file `x\y.jpg` onto `x/y.jpg`.
func SafeJoinRel(baseDir, relPosix string) (string, error) {
	if filepath.Separator == '\\' {
		relPosix = strings.ReplaceAll(relPosix, "\\", "/")
	}
	cleaned := path.Clean("/" + relPosix)
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
	Sidecar *Sidecar
	// SourcePath is the file the state came from: the canonical sidecar when
	// it exists, otherwise the copy beside the photo.
	SourcePath  string
	Quarantined string
	// MergedFrom is the beside-the-photo copy that was joined into the
	// canonical sidecar, when both layouts hold truth for the asset.
	MergedFrom string
}

// Read loads an asset's truth from both on-disk layouts and joins them:
//
//   - <sidecarRoot>/<rel>.edits, the canonical sidecar this server writes
//   - beside the photo: <folderRoot>/<rel>.edits, else <folderRoot>/<rel>.hdrt.json
//
// A browser editor can be configured to keep sidecars beside the photos, and
// older ones always did, so a folder may hold a copy in each layout. Reading
// only the canonical one would show stale state whenever the other is newer.
// The join is Merge with conflict rescue, exactly what the browser editor
// computes for the same two files, so both tools show identical state. The
// copy beside the photo is never written, moved, or removed.
//
// If the canonical .edits file exists on disk but fails parsing, it is moved
// to <sidecarRoot>/.quarantine/ and NOT silently masked by the other copy.
func (s *Store) Read(folderRoot, sidecarRoot, relPosix string) (*ReadResult, error) {
	canonical, err := CanonicalEditsPath(sidecarRoot, relPosix)
	if err != nil {
		return nil, err
	}
	var inFolder *Sidecar
	if data, err := os.ReadFile(canonical); err == nil {
		sc, parseErr := Parse(data)
		if parseErr != nil {
			qPath, qErr := quarantineFile(sidecarRoot, relPosix, canonical)
			if qErr != nil {
				return nil, fmt.Errorf("corrupt sidecar %s (quarantine failed: %w): %w", canonical, qErr, parseErr)
			}
			return &ReadResult{Quarantined: qPath}, nil
		}
		inFolder = sc
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	beside, besidePath := readBesidePhoto(folderRoot, relPosix)
	switch {
	case inFolder == nil && beside == nil:
		return &ReadResult{}, nil
	case inFolder == nil:
		return &ReadResult{Sidecar: beside, SourcePath: besidePath}, nil
	case beside == nil:
		return &ReadResult{Sidecar: inFolder, SourcePath: canonical}, nil
	}
	merged, err := Merge(inFolder, beside, MergeOptions{
		RescueConflictingRecipes: true,
		OriginA:                  OriginSidecarFolder,
		OriginB:                  OriginBesidePhoto,
	})
	if err != nil {
		return nil, fmt.Errorf("merge %s with %s: %w", canonical, besidePath, err)
	}
	return &ReadResult{Sidecar: merged, SourcePath: canonical, MergedFrom: besidePath}, nil
}

// besidePhotoCandidates lists the read-only sidecar names beside a photo, in
// precedence order.
func besidePhotoCandidates(folderRoot, relPosix string) []string {
	var out []string
	for _, suffix := range []string{".edits", ".hdrt.json"} {
		if p, err := SafeJoinRel(folderRoot, relPosix+suffix); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// readBesidePhoto returns the first readable truth sidecar beside the photo.
// These files are never written to. A portable document (an explicitly shared
// edit file that happens to sit there) is not folder truth and is skipped.
func readBesidePhoto(folderRoot, relPosix string) (*Sidecar, string) {
	for _, p := range besidePhotoCandidates(folderRoot, relPosix) {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		sc, err := Parse(data)
		if err != nil {
			continue
		}
		if sc.Document == nil {
			sc.Document = &DocumentMeta{Role: "truth"}
		}
		if sc.Document.Role != "truth" {
			continue
		}
		return sc, p
	}
	return nil, ""
}

// ChangeStamp summarizes the modification state of every file Read consults
// for an asset, so an index can tell when its projection is stale. It equals
// the canonical sidecar's mtime (0 when absent) while no copy exists beside
// the photo.
func ChangeStamp(folderRoot, sidecarRoot, relPosix string) int64 {
	var stamp uint64
	if canonical, err := CanonicalEditsPath(sidecarRoot, relPosix); err == nil {
		if info, err := os.Stat(canonical); err == nil {
			stamp = uint64(info.ModTime().UTC().UnixNano())
		}
	}
	for i, p := range besidePhotoCandidates(folderRoot, relPosix) {
		if info, err := os.Stat(p); err == nil {
			// Odd-forced so a present file always perturbs the stamp.
			mixed := (uint64(info.ModTime().UTC().UnixNano()) ^ (uint64(info.Size()) * 0x9e3779b97f4a7c15)) | 1
			stamp ^= bits.RotateLeft64(mixed, 7*(i+1))
		}
	}
	return int64(stamp)
}

var (
	strictFsync = strings.TrimSpace(os.Getenv("CLOUDEDITS_FSYNC")) == "1"
	ensuredDirs sync.Map
)

func ensureDir(dir string) error {
	if _, ok := ensuredDirs.Load(dir); ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ensuredDirs.Store(dir, struct{}{})
	return nil
}

// WriteAtomic writes data atomically using <sidecarRoot>/.tmp/<uuid> -> write
// -> close -> rename, creating parent directories as needed. Matches SQLite's
// PRAGMA synchronous=NORMAL durability tier by default (100% atomic against
// concurrent readers and process crashes); set CLOUDEDITS_FSYNC=1 to force a
// per-write hardware fsync barrier before rename.
func WriteAtomic(sidecarRoot, targetPath string, data []byte) error {
	tmpDir := filepath.Join(sidecarRoot, ".tmp")
	if err := ensureDir(tmpDir); err != nil {
		return fmt.Errorf("create .tmp dir: %w", err)
	}
	targetDir := filepath.Dir(targetPath)
	if err := ensureDir(targetDir); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	tmpPath := filepath.Join(tmpDir, randomHex(16)+".tmp")
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		// Directory may have been removed externally; retry MkdirAll once.
		ensuredDirs.Delete(tmpDir)
		ensuredDirs.Delete(targetDir)
		if mkErr := os.MkdirAll(tmpDir, 0o755); mkErr != nil {
			return fmt.Errorf("create .tmp dir: %w", mkErr)
		}
		if mkErr := os.MkdirAll(targetDir, 0o755); mkErr != nil {
			return fmt.Errorf("create target dir: %w", mkErr)
		}
		f, err = os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("create staging file: %w", err)
		}
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
	if strictFsync {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("fsync staging file: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close staging file: %w", err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		// Target parent directory may have been removed externally; ensure and retry once.
		ensuredDirs.Delete(targetDir)
		if mkErr := os.MkdirAll(targetDir, 0o755); mkErr == nil {
			if err2 := os.Rename(tmpPath, targetPath); err2 == nil {
				cleanup = false
				return nil
			}
		}
		return fmt.Errorf("atomic rename: %w", err)
	}
	cleanup = false
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
	sc, changed, _, err := s.UpdateWithGen(folderRoot, sidecarRoot, relPosix, sizeBytes, contentHash, mutate)
	return sc, changed, err
}

// UpdateWithGen is like Update and additionally returns a per-asset monotonic
// generation counter assigned under the asset's mutex, allowing downstream
// index projections to avoid overwriting newer state with older snapshots when
// unrelated writers commit concurrently.
func (s *Store) UpdateWithGen(
	folderRoot, sidecarRoot, relPosix string,
	sizeBytes *int64,
	contentHash string,
	mutate func(sc *Sidecar) (changed bool, err error),
) (*Sidecar, bool, uint64, error) {
	canonical, err := CanonicalEditsPath(sidecarRoot, relPosix)
	if err != nil {
		return nil, false, 0, err
	}

	lk := s.lockFor(canonical)
	lk.mu.Lock()
	defer lk.mu.Unlock()

	res, err := s.Read(folderRoot, sidecarRoot, relPosix)
	if err != nil {
		return nil, false, 0, err
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
		return nil, false, 0, err
	}
	if !changed {
		return sc, false, lk.gen, nil
	}

	payload, err := Serialize(sc)
	if err != nil {
		return nil, false, 0, err
	}
	if err := WriteAtomic(sidecarRoot, canonical, payload); err != nil {
		return nil, false, 0, err
	}
	lk.gen++
	s.writeCount.Add(1)
	return sc, true, lk.gen, nil
}

// WriteEditThumbnail atomically writes an edited preview JPEG to
// <sidecarRoot>/<relPosix>.edit_thumbnail.jpg.
func (s *Store) WriteEditThumbnail(sidecarRoot, relPosix string, jpegBytes []byte) (string, error) {
	target, err := EditThumbnailPath(sidecarRoot, relPosix)
	if err != nil {
		return "", err
	}
	lk := s.lockFor(target)
	lk.mu.Lock()
	defer lk.mu.Unlock()

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
	lk.mu.Lock()
	defer lk.mu.Unlock()

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
