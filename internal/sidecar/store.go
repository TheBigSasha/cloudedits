package sidecar

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/thebigsasha/cloudedits/internal/formats"
	"github.com/thebigsasha/cloudedits/internal/safefile"
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
	return SafeJoinRel(sidecarRoot, relPosix+formats.EditThumbnailSuffix)
}

// ErrInvalidAuxChannel indicates an empty, syntactically invalid, or
// unrecognized auxiliary channel name.
var ErrInvalidAuxChannel = errors.New("invalid auxiliary channel")

// ErrUnsupportedAuxFormat indicates an auxiliary channel payload whose format
// or magic bytes are not permitted for that channel.
var ErrUnsupportedAuxFormat = errors.New("unsupported auxiliary channel format")

// ErrSidecarTooLarge indicates a write that would grow a .edits document past
// MaxSidecarBytes.
var ErrSidecarTooLarge = errors.New("sidecar exceeds size limit")

const (
	// MaxSidecarBytes bounds a .edits document, on read and on write. Real
	// documents are kilobytes; the bound keeps a hostile or runaway file from
	// being parsed into memory (the JSON tree is several times its size).
	MaxSidecarBytes = 16 * 1024 * 1024
	// MaxAuxChannelBytes bounds an auxiliary channel image (JPEG or PNG), on
	// upload and on read.
	MaxAuxChannelBytes = 16 * 1024 * 1024
)

var auxChannelNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ValidateAuxChannel validates channel against the safe channel name syntax
// (^[a-z][a-z0-9_-]{0,31}$) and the explicit recognized channel -> suffix
// mapping, returning the legacy default suffix (e.g. ".depth.png") or
// ErrInvalidAuxChannel.
func ValidateAuxChannel(channel string) (string, error) {
	if !auxChannelNamePattern.MatchString(channel) {
		return "", fmt.Errorf("%w: %q", ErrInvalidAuxChannel, channel)
	}
	suffix, ok := formats.AuxChannelSuffix(channel)
	if !ok {
		return "", fmt.Errorf("%w: unsupported channel %q", ErrInvalidAuxChannel, channel)
	}
	return suffix, nil
}

// AuxChannelPath returns <sidecarRoot>/<relPosix>.<channel>.png for a valid
// auxiliary channel. Prefer AuxChannelPathForFormat when selecting between
// JPEG and PNG representations.
func AuxChannelPath(sidecarRoot, relPosix, channel string) (string, error) {
	suffix, err := ValidateAuxChannel(channel)
	if err != nil {
		return "", err
	}
	return SafeJoinRel(sidecarRoot, relPosix+suffix)
}

// AuxChannelPathForFormat returns <sidecarRoot>/<relPosix><suffix> for a valid
// auxiliary channel and format ("jpg"/"jpeg"/"image/jpeg" or "png"/"image/png").
func AuxChannelPathForFormat(sidecarRoot, relPosix, channel, format string) (string, error) {
	if _, err := ValidateAuxChannel(channel); err != nil {
		return "", err
	}
	suffix, ok := formats.AuxChannelSuffixForFormat(channel, format)
	if !ok {
		return "", fmt.Errorf("%w: %q for channel %q", ErrUnsupportedAuxFormat, format, channel)
	}
	return SafeJoinRel(sidecarRoot, relPosix+suffix)
}

// auxChannelLockKey returns the single per-asset, per-channel mutex key shared
// across all on-disk formats of channel (e.g. both .depth.jpg and .depth.png).
func auxChannelLockKey(sidecarRoot, relPosix, channel string) (string, error) {
	if _, err := ValidateAuxChannel(channel); err != nil {
		return "", err
	}
	base, err := SafeJoinRel(sidecarRoot, relPosix)
	if err != nil {
		return "", err
	}
	return "aux:" + base + "\x00" + channel, nil
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

	// canonicalRaw is the exact content of the canonical sidecar this read
	// saw (nil when it was absent or quarantined). A writer compares it with
	// the file again just before replacing it.
	canonicalRaw []byte
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
	var (
		inFolder     *Sidecar
		canonicalRaw []byte
	)
	if data, err := readRegularUnderRoot(sidecarRoot, canonical, MaxSidecarBytes); err == nil {
		sc, parseErr := Parse(data)
		if parseErr != nil {
			qPath, qErr := quarantineFile(sidecarRoot, relPosix, canonical)
			if qErr != nil {
				return nil, fmt.Errorf("corrupt sidecar %s (quarantine failed: %w): %w", canonical, qErr, parseErr)
			}
			return &ReadResult{Quarantined: qPath}, nil
		}
		inFolder, canonicalRaw = sc, data
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
		return &ReadResult{Sidecar: inFolder, SourcePath: canonical, canonicalRaw: canonicalRaw}, nil
	}
	merged, err := Merge(inFolder, beside, MergeOptions{
		RescueConflictingRecipes: true,
		OriginA:                  OriginSidecarFolder,
		OriginB:                  OriginBesidePhoto,
	})
	if err != nil {
		return nil, fmt.Errorf("merge %s with %s: %w", canonical, besidePath, err)
	}
	return &ReadResult{Sidecar: merged, SourcePath: canonical, MergedFrom: besidePath, canonicalRaw: canonicalRaw}, nil
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
		data, _, err := safefile.ReadRegular(p, MaxSidecarBytes)
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

// EditThumbnailStamp identifies the current content of an asset's edited
// preview by modification time and size, or 0 when there is none. It changes
// whenever the preview is replaced. Uses os.Root + Lstat so symbolic links
// (either the file itself or a parent directory escaping sidecarRoot) report 0,
// matching EditThumbnailStamps and thumb.Service.
func EditThumbnailStamp(sidecarRoot, relPosix string) int64 {
	target, err := EditThumbnailPath(sidecarRoot, relPosix)
	if err != nil {
		return 0
	}
	rel, err := relWithinRoot(sidecarRoot, target)
	if err != nil {
		return 0
	}
	root, err := openSidecarRoot(sidecarRoot, false, false)
	if err != nil {
		return 0
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(rel)
	if err != nil {
		return 0
	}
	return editThumbnailStampOf(info)
}

func editThumbnailStampOf(info os.FileInfo) int64 {
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return 0
	}
	// Odd-forced so an existing preview never stamps as 0.
	return int64((uint64(info.ModTime().UTC().UnixNano()) ^ (uint64(info.Size()) * 0x9e3779b97f4a7c15)) | 1)
}

// EditThumbnailStamps returns the stamp of every edited preview stored under
// sidecarRoot, keyed by the asset's POSIX relative path. It lists directories
// and stats only the previews, so its cost follows the number of edited
// photos, not the size of the library.
func EditThumbnailStamps(sidecarRoot string) map[string]int64 {
	out := map[string]int64{}
	_ = filepath.WalkDir(sidecarRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry only hides its own previews
		}
		if d.IsDir() {
			if p != sidecarRoot && filepath.Dir(p) == sidecarRoot {
				switch d.Name() {
				case ".tmp", ".quarantine", "renders":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), formats.EditThumbnailSuffix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // vanished between listing and stat
		}
		rel, err := filepath.Rel(sidecarRoot, p)
		if err != nil {
			return nil //nolint:nilerr // cannot happen for a path under the walked root
		}
		if stamp := editThumbnailStampOf(info); stamp != 0 {
			out[strings.TrimSuffix(filepath.ToSlash(rel), formats.EditThumbnailSuffix)] = stamp
		}
		return nil
	})
	return out
}

// staleStagingAge is how old an orphaned staging file must be before it is
// swept. It is far longer than any single write, so a file another process is
// still writing is never touched.
const staleStagingAge = 15 * time.Minute

var (
	// durableWrites flushes every sidecar and render write to stable storage
	// before it is acknowledged. CLOUDEDITS_FSYNC=0 trades that guarantee for
	// latency: writes stay atomic against crashes of this process, but an OS
	// crash or power loss can lose or truncate an acknowledged write.
	durableWrites = strings.TrimSpace(os.Getenv("CLOUDEDITS_FSYNC")) != "0"
	ensuredDirs   sync.Map

	// Seams so tests can observe the flush sequence.
	syncFile = func(f *os.File) error { return f.Sync() }
	syncDir  = syncDirectory
)

// syncDirectory flushes a directory's entries (a completed rename or a newly
// created child) to stable storage.
func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		// A directory handle cannot be flushed on Windows; NTFS journals the
		// rename as part of the metadata transaction.
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	// Some network and FUSE filesystems do not implement directory fsync.
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) && !errors.Is(syncErr, errors.ErrUnsupported) {
		return syncErr
	}
	return closeErr
}

// openSidecarRoot opens sidecarRoot as an os.Root so every write, rename,
// remove, and read beneath it is confined to the sidecar tree and cannot
// follow symlinked directories outside sidecarRoot. A sidecarRoot that is
// itself a symbolic link is refused.
func openSidecarRoot(sidecarRoot string, create, durable bool) (*os.Root, error) {
	info, err := os.Lstat(sidecarRoot)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("sidecar root %q must not be a symbolic link", sidecarRoot)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("sidecar root %q is not a directory", sidecarRoot)
		}
	case errors.Is(err, os.ErrNotExist):
		if !create {
			return nil, err
		}
		if err := os.MkdirAll(sidecarRoot, 0o755); err != nil {
			return nil, err
		}
		if durable {
			if err := syncDir(filepath.Dir(sidecarRoot)); err != nil {
				return nil, err
			}
		}
	default:
		return nil, err
	}
	return os.OpenRoot(sidecarRoot)
}

// relWithinRoot returns targetPath relative to sidecarRoot and rejects any
// path that does not stay strictly beneath sidecarRoot.
func relWithinRoot(sidecarRoot, targetPath string) (string, error) {
	absRoot, err := filepath.Abs(sidecarRoot)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return "", err
	}
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q escapes sidecar root %q", targetPath, sidecarRoot)
	}
	return rel, nil
}

// readRegularUnderRoot reads targetPath strictly beneath sidecarRoot using os.Root.
func readRegularUnderRoot(sidecarRoot, targetPath string, limit int64) ([]byte, error) {
	rel, err := relWithinRoot(sidecarRoot, targetPath)
	if err != nil {
		return nil, err
	}
	root, err := openSidecarRoot(sidecarRoot, false, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	data, _, err := safefile.ReadRegularInRoot(root, rel, limit)
	return data, err
}

// ensureDirInRoot creates relDir (and missing parents) strictly inside root.
// When it had to create directories under a durable write, the newly created
// parent directory entries are flushed up to sidecarRoot.
func ensureDirInRoot(root *os.Root, sidecarRoot, relDir string, durable bool) error {
	if relDir == "" || relDir == "." {
		return nil
	}
	fullDir := filepath.Join(sidecarRoot, relDir)
	_, statErr := root.Stat(relDir)
	if statErr == nil {
		ensuredDirs.Store(fullDir, struct{}{})
		return nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		// Propagate root-escape or permission errors instead of masking them.
		return statErr
	}
	if err := root.MkdirAll(relDir, 0o755); err != nil {
		return err
	}
	if durable {
		for p := fullDir; ; p = filepath.Dir(p) {
			parent := filepath.Dir(p)
			if parent == p {
				break
			}
			if err := syncDir(parent); err != nil {
				return err
			}
			if p == sidecarRoot {
				break
			}
		}
	}
	ensuredDirs.Store(fullDir, struct{}{})
	return nil
}

// sweepStaleStagingInRoot removes staging files orphaned by a process that died
// between creating one and renaming it into place. Only this server's own
// "<32 hex>.tmp" names are considered, and only once they are old.
func sweepStaleStagingInRoot(root *os.Root) {
	d, err := root.Open(".tmp")
	if err != nil {
		return
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleStagingAge)
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || len(name) != 32+len(".tmp") || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		if _, err := hex.DecodeString(strings.TrimSuffix(name, ".tmp")); err != nil {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = root.Remove(filepath.Join(".tmp", name))
		}
	}
}

// WriteAtomic durably replaces targetPath with data: stage in
// <sidecarRoot>/.tmp/<uuid>, write, fsync, close, rename, fsync the target
// directory. A reader sees the old bytes or the new bytes, never a mixture,
// and once WriteAtomic returns the new bytes survive power loss. All directory
// creation, staging, and renaming happen through os.Root so symlinked
// directories inside sidecarRoot cannot escape it.
func WriteAtomic(sidecarRoot, targetPath string, data []byte) error {
	return writeAtomic(sidecarRoot, targetPath, data, durableWrites)
}

func writeAtomic(sidecarRoot, targetPath string, data []byte, durable bool) error {
	return writeAtomicGuarded(sidecarRoot, targetPath, data, durable, nil)
}

// writeAtomicGuarded is writeAtomic with a last-moment veto: precommit runs
// after the bytes are staged and flushed, immediately before the rename. When
// it returns an error nothing is replaced and that error is returned.
func writeAtomicGuarded(sidecarRoot, targetPath string, data []byte, durable bool, precommit func() error) error {
	relTarget, err := relWithinRoot(sidecarRoot, targetPath)
	if err != nil {
		return err
	}
	root, err := openSidecarRoot(sidecarRoot, true, durable)
	if err != nil {
		return fmt.Errorf("open sidecar root: %w", err)
	}
	defer func() { _ = root.Close() }()

	tmpDir := filepath.Join(sidecarRoot, ".tmp")
	if _, known := ensuredDirs.Load(tmpDir); !known {
		sweepStaleStagingInRoot(root)
	}
	if err := ensureDirInRoot(root, sidecarRoot, ".tmp", durable); err != nil {
		return fmt.Errorf("create .tmp dir: %w", err)
	}
	relTargetDir := filepath.Dir(relTarget)
	if err := ensureDirInRoot(root, sidecarRoot, relTargetDir, durable); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	tmpRel := filepath.Join(".tmp", randomHex(16)+".tmp")
	f, err := root.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		// Directory may have been removed externally; recreate and retry once.
		ensuredDirs.Delete(tmpDir)
		ensuredDirs.Delete(filepath.Join(sidecarRoot, relTargetDir))
		if mkErr := ensureDirInRoot(root, sidecarRoot, ".tmp", durable); mkErr != nil {
			return fmt.Errorf("create .tmp dir: %w", mkErr)
		}
		if mkErr := ensureDirInRoot(root, sidecarRoot, relTargetDir, durable); mkErr != nil {
			return fmt.Errorf("create target dir: %w", mkErr)
		}
		f, err = root.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	}
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}

	renamed := false
	defer func() {
		if !renamed {
			_ = root.Remove(tmpRel)
		}
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write staging file: %w", err)
	}
	if durable {
		if err := syncFile(f); err != nil {
			_ = f.Close()
			return fmt.Errorf("fsync staging file: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close staging file: %w", err)
	}

	if precommit != nil {
		if err := precommit(); err != nil {
			return err
		}
	}
	if err := root.Rename(tmpRel, relTarget); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Target parent directory may have been removed externally; recreate and retry once.
			ensuredDirs.Delete(filepath.Join(sidecarRoot, relTargetDir))
			if mkErr := ensureDirInRoot(root, sidecarRoot, relTargetDir, durable); mkErr != nil {
				return fmt.Errorf("atomic rename: %w", mkErr)
			}
			if err2 := root.Rename(tmpRel, relTarget); err2 != nil {
				return fmt.Errorf("atomic rename: %w", err2)
			}
		} else {
			return fmt.Errorf("atomic rename: %w", err)
		}
	}
	renamed = true
	if durable {
		if err := syncDir(filepath.Join(sidecarRoot, relTargetDir)); err != nil {
			return fmt.Errorf("fsync target directory: %w", err)
		}
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

	// Another process can own the same folder: a browser editor with direct
	// file access writes this very file with its own read-merge-write. Staging
	// (write + flush) takes long enough for such a write to land after this
	// update's read; renaming over it would discard an edit that process was
	// told is saved. So the file is compared with what was read immediately
	// before the rename, and a changed file restarts the read-merge-write.
	for attempt := 1; ; attempt++ {
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
		if len(payload) > MaxSidecarBytes {
			// Never write a document Read would refuse: that would strand the
			// asset's edits on disk where no later op could reach them.
			return nil, false, 0, fmt.Errorf("%w: %s would be %d bytes (limit %d)", ErrSidecarTooLarge, canonical, len(payload), MaxSidecarBytes)
		}
		err = writeAtomicGuarded(sidecarRoot, canonical, payload, durableWrites, func() error {
			return canonicalStillHolds(sidecarRoot, canonical, res.canonicalRaw)
		})
		if errors.Is(err, errCanonicalMoved) {
			if attempt < maxCommitAttempts {
				continue
			}
			return nil, false, 0, fmt.Errorf("%w: %s changed during %d consecutive writes", ErrContended, canonical, attempt)
		}
		if err != nil {
			return nil, false, 0, err
		}
		lk.gen++
		s.writeCount.Add(1)
		return sc, true, lk.gen, nil
	}
}

// maxCommitAttempts bounds how often one update restarts because another
// process replaced the sidecar under it.
const maxCommitAttempts = 6

// ErrContended reports that another process kept rewriting a sidecar for the
// whole of an update, so nothing was written. The update can be retried.
var ErrContended = errors.New("sidecar is being written by another process")

var errCanonicalMoved = errors.New("canonical sidecar changed since it was read")

// canonicalStillHolds reports errCanonicalMoved unless the canonical sidecar
// still has exactly the content seen (nil: it did not exist).
func canonicalStillHolds(sidecarRoot, canonical string, seen []byte) error {
	now, err := readRegularUnderRoot(sidecarRoot, canonical, MaxSidecarBytes)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if seen == nil {
			return nil
		}
		return errCanonicalMoved
	case err != nil:
		return err
	case seen == nil || !bytes.Equal(now, seen):
		return errCanonicalMoved
	}
	return nil
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

	// A disposable, rebuildable preview: atomic, but not worth a flush.
	if err := writeAtomic(sidecarRoot, target, jpegBytes, false); err != nil {
		return "", err
	}
	return target, nil
}

// auxChannelMagicMatchesSuffix verifies that data starts with the image
// signature corresponding to suffix (".jpg" -> JPEG SOI, ".png" -> PNG magic).
func auxChannelMagicMatchesSuffix(suffix string, data []byte) bool {
	switch {
	case strings.HasSuffix(suffix, ".jpg"):
		return formats.IsJPEGMagic(data)
	case strings.HasSuffix(suffix, ".png"):
		return formats.IsPNGMagic(data)
	default:
		return false
	}
}

// ReadAuxChannel loads an auxiliary channel sidecar for an asset, checking the
// canonical paths under sidecarRoot (e.g. .depth.jpg and .depth.png via os.Root,
// preferring the newest mtime if both exist) first, then falling back to a
// read-only colocated copy beside the photo under folderRoot (also preferring
// newest mtime if both exist). Returns os.ErrNotExist when no valid copy exists,
// or ErrInvalidAuxChannel when channel is not allowed.
func (s *Store) ReadAuxChannel(folderRoot, sidecarRoot, relPosix, channel string) ([]byte, string, error) {
	lockKey, err := auxChannelLockKey(sidecarRoot, relPosix, channel)
	if err != nil {
		return nil, "", err
	}
	suffixes, ok := formats.AuxChannelSuffixes(channel)
	if !ok {
		return nil, "", fmt.Errorf("%w: unsupported channel %q", ErrInvalidAuxChannel, channel)
	}
	lk := s.lockFor(lockKey)
	lk.mu.Lock()
	defer lk.mu.Unlock()

	if data, src, err := readNewestAuxChannelUnderRoot(sidecarRoot, relPosix, suffixes); err == nil {
		return data, src, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}

	if folderRoot != "" {
		if data, src, err := readNewestAuxChannelColocated(folderRoot, relPosix, suffixes); err == nil {
			return data, src, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
	}
	return nil, "", os.ErrNotExist
}

// readNewestAuxChannelUnderRoot scans all recognized format suffixes for a
// channel strictly beneath sidecarRoot via os.Root, returning the valid regular
// file with the newest ModTime (ties broken by suffixes slice order).
func readNewestAuxChannelUnderRoot(sidecarRoot, relPosix string, suffixes []string) ([]byte, string, error) {
	root, err := openSidecarRoot(sidecarRoot, false, false)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = root.Close() }()

	var bestData []byte
	var bestPath string
	var bestMod time.Time
	found := false

	for _, suffix := range suffixes {
		canonical, err := SafeJoinRel(sidecarRoot, relPosix+suffix)
		if err != nil {
			return nil, "", err
		}
		rel, err := relWithinRoot(sidecarRoot, canonical)
		if err != nil {
			return nil, "", err
		}
		data, info, err := readAuxChannelFileInRoot(root, rel, suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if !found || info.ModTime().After(bestMod) {
			bestData = data
			bestPath = canonical
			bestMod = info.ModTime()
			found = true
		}
	}
	if !found {
		return nil, "", os.ErrNotExist
	}
	return bestData, bestPath, nil
}

// readNewestAuxChannelColocated scans read-only colocated auxiliary channel
// files beside the source photo, returning the valid regular file with the
// newest ModTime (ties broken by suffixes slice order).
func readNewestAuxChannelColocated(folderRoot, relPosix string, suffixes []string) ([]byte, string, error) {
	var bestData []byte
	var bestPath string
	var bestMod time.Time
	found := false

	for _, suffix := range suffixes {
		beside, err := SafeJoinRel(folderRoot, relPosix+suffix)
		if err != nil {
			continue
		}
		data, info, err := readAuxChannelFile(beside, suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if !found || info.ModTime().After(bestMod) {
			bestData = data
			bestPath = beside
			bestMod = info.ModTime()
			found = true
		}
	}
	if !found {
		return nil, "", os.ErrNotExist
	}
	return bestData, bestPath, nil
}

// readAuxChannelFileInRoot reads an auxiliary channel file strictly beneath
// root via os.Root, rejecting symlinked directories escaping sidecarRoot as
// well as non-regular, oversized, or magic-mismatched files.
func readAuxChannelFileInRoot(root *os.Root, rel, suffix string) ([]byte, os.FileInfo, error) {
	data, info, err := safefile.ReadRegularInRoot(root, rel, MaxAuxChannelBytes)
	if errors.Is(err, safefile.ErrNotRegular) || errors.Is(err, safefile.ErrTooLarge) {
		return nil, nil, os.ErrNotExist
	}
	if err != nil {
		return nil, nil, err
	}
	if !auxChannelMagicMatchesSuffix(suffix, data) {
		return nil, nil, os.ErrNotExist
	}
	return data, info, nil
}

// readAuxChannelFile returns an auxiliary channel file only if it is something
// the server would itself have stored: a regular file within the upload limit
// whose leading bytes match suffix's expected image signature (JPEG or PNG).
// Anything else found at that name (a symbolic link, a device, an oversized or
// mismatched file) is reported as absent rather than followed, read into
// memory, or served as an image.
func readAuxChannelFile(path, suffix string) ([]byte, os.FileInfo, error) {
	data, info, err := safefile.ReadRegular(path, MaxAuxChannelBytes)
	if errors.Is(err, safefile.ErrNotRegular) || errors.Is(err, safefile.ErrTooLarge) {
		return nil, nil, os.ErrNotExist
	}
	if err != nil {
		return nil, nil, err
	}
	if !auxChannelMagicMatchesSuffix(suffix, data) {
		return nil, nil, os.ErrNotExist
	}
	return data, info, nil
}

// WriteAuxChannel atomically commits an auxiliary channel sidecar (JPEG ->
// .<channel>.jpg or PNG -> .<channel>.png) to <sidecarRoot> via
// <sidecarRoot>/.tmp/<uuid> -> Sync() -> Close() -> os.Rename(), and removes
// any stale opposite-format sibling under the same asset-level channel lock.
// Source photo folders are never modified.
func (s *Store) WriteAuxChannel(sidecarRoot, relPosix, channel string, data []byte) (string, error) {
	lockKey, err := auxChannelLockKey(sidecarRoot, relPosix, channel)
	if err != nil {
		return "", err
	}
	suffix, _, ok := formats.AuxChannelSuffixForData(channel, data)
	if len(data) > MaxAuxChannelBytes || !ok {
		return "", fmt.Errorf("%w: auxiliary channel must be a JPEG or PNG of at most %d bytes", ErrUnsupportedAuxFormat, MaxAuxChannelBytes)
	}
	target, err := SafeJoinRel(sidecarRoot, relPosix+suffix)
	if err != nil {
		return "", err
	}
	allSuffixes, _ := formats.AuxChannelSuffixes(channel)

	lk := s.lockFor(lockKey)
	lk.mu.Lock()
	defer lk.mu.Unlock()

	if err := WriteAtomic(sidecarRoot, target, data); err != nil {
		return "", err
	}

	// Clean up any stale opposite-format sibling (e.g. remove old .depth.png
	// after committing .depth.jpg) strictly under os.Root.
	root, err := openSidecarRoot(sidecarRoot, false, false)
	if err != nil {
		return target, err
	}
	defer func() { _ = root.Close() }()

	removedSibling := false
	var syncTargetDir string
	for _, otherSuffix := range allSuffixes {
		if otherSuffix == suffix {
			continue
		}
		otherPath, err := SafeJoinRel(sidecarRoot, relPosix+otherSuffix)
		if err != nil {
			return target, err
		}
		relOther, err := relWithinRoot(sidecarRoot, otherPath)
		if err != nil {
			return target, err
		}
		if err := root.Remove(relOther); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return target, err
		}
		removedSibling = true
		syncTargetDir = filepath.Join(sidecarRoot, filepath.Dir(relOther))
	}
	if removedSibling && durableWrites {
		if err := syncDir(syncTargetDir); err != nil {
			return target, fmt.Errorf("fsync target directory after stale format cleanup: %w", err)
		}
	}
	return target, nil
}

// DeleteAuxChannel removes all disposable format variants (e.g. .depth.jpg and
// .depth.png) of an auxiliary channel sidecar under sidecarRoot. It never
// touches files in the source photo folder. Reports whether any file was
// removed.
func (s *Store) DeleteAuxChannel(sidecarRoot, relPosix, channel string) (bool, error) {
	lockKey, err := auxChannelLockKey(sidecarRoot, relPosix, channel)
	if err != nil {
		return false, err
	}
	suffixes, ok := formats.AuxChannelSuffixes(channel)
	if !ok {
		return false, fmt.Errorf("%w: unsupported channel %q", ErrInvalidAuxChannel, channel)
	}
	lk := s.lockFor(lockKey)
	lk.mu.Lock()
	defer lk.mu.Unlock()

	root, err := openSidecarRoot(sidecarRoot, false, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = root.Close() }()

	deleted := false
	var syncTargetDir string
	for _, suffix := range suffixes {
		target, err := SafeJoinRel(sidecarRoot, relPosix+suffix)
		if err != nil {
			return deleted, err
		}
		relTarget, err := relWithinRoot(sidecarRoot, target)
		if err != nil {
			return deleted, err
		}
		if err := root.Remove(relTarget); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return deleted, err
		}
		deleted = true
		syncTargetDir = filepath.Join(sidecarRoot, filepath.Dir(relTarget))
	}
	if deleted && durableWrites {
		if err := syncDir(syncTargetDir); err != nil {
			return true, fmt.Errorf("fsync target directory after delete: %w", err)
		}
	}
	return deleted, nil
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

	relWithin, err := relWithinRoot(sidecarRoot, target)
	if err != nil {
		return nil, false, err
	}
	if root, rerr := openSidecarRoot(sidecarRoot, false, false); rerr == nil {
		info, serr := root.Stat(relWithin)
		_ = root.Close()
		if serr == nil {
			return &RenderEntry{
				RenderID:  cleanID,
				VariantID: variantID,
				FileName:  filepath.Base(target),
				MIME:      mimeType,
				SizeBytes: info.Size(),
				UpdatedAt: info.ModTime().UTC().Format(time.RFC3339),
			}, false, nil
		}
		if !errors.Is(serr, os.ErrNotExist) {
			return nil, false, serr
		}
	} else if !errors.Is(rerr, os.ErrNotExist) {
		return nil, false, rerr
	}

	if err := WriteAtomic(sidecarRoot, target, data); err != nil {
		return nil, false, err
	}
	root, err := openSidecarRoot(sidecarRoot, false, false)
	if err != nil {
		return nil, false, err
	}
	info, err := root.Stat(relWithin)
	_ = root.Close()
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
	relDir, err := relWithinRoot(sidecarRoot, dir)
	if err != nil {
		return nil, err
	}
	root, err := openSidecarRoot(sidecarRoot, false, false)
	if errors.Is(err, os.ErrNotExist) {
		return []RenderEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	d, err := root.Open(relDir)
	if errors.Is(err, os.ErrNotExist) {
		return []RenderEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
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
	srcRel, err := relWithinRoot(sidecarRoot, srcPath)
	if err != nil {
		return "", err
	}
	dstRel, err := relWithinRoot(sidecarRoot, dstPath)
	if err != nil {
		return "", err
	}
	root, err := openSidecarRoot(sidecarRoot, false, false)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll(".quarantine", 0o755); err != nil {
		return "", err
	}
	if err := root.Rename(srcRel, dstRel); err != nil {
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
