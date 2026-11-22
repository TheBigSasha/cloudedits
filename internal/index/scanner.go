package index

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/formats"
	"github.com/thebigsasha/cloudedits/internal/hlc"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
	"github.com/thebigsasha/cloudedits/internal/thumb"
)

// ScanAll performs a read-only walk of all configured folders, indexing media
// files and their .hdrtoys/**/*.edits sidecars, and pruning deleted entries.
func (idx *Index) ScanAll(ctx context.Context) error {
	folders := idx.Folders()
	for _, f := range folders {
		if err := idx.ScanFolder(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

// ScanFolder scans a single configured folder strictly read-only.
func (idx *Index) ScanFolder(ctx context.Context, fc config.FolderConfig) error {
	sidecarRoot := sidecar.SidecarRoot(fc.Path, fc.ID, idx.sidecarDir)
	seen := map[string]bool{}

	err := filepath.WalkDir(fc.Path, func(p string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			// Skip unreadable entries during best-effort read-only walk.
			return nil //nolint:nilerr // unreadable sub-entries must not abort full folder scan
		}
		if p == fc.Path {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if formats.ShouldSkipName(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isSupported(name) {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return nil //nolint:nilerr // skip vanished or unreadable entries cleanly
		}
		rel, relErr := filepath.Rel(fc.Path, p)
		if relErr != nil {
			return nil //nolint:nilerr // skip paths that cannot be made relative
		}
		relPosix := filepath.ToSlash(rel)
		assetID := MakeAssetID(fc.ID, relPosix)
		seen[assetID] = true

		return idx.indexOneFile(ctx, fc, sidecarRoot, p, relPosix, info)
	})
	if err != nil {
		return err
	}

	// Prune assets belonging to this folder that no longer exist on disk.
	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()

	rows, err := idx.db.QueryContext(ctx, `SELECT id FROM assets WHERE folder_id = ?`, fc.ID)
	if err != nil {
		return err
	}
	var staleIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil && !seen[id] {
			staleIDs = append(staleIDs, id)
		}
	}
	_ = rows.Close()

	if len(staleIDs) > 0 {
		tx, err := idx.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, id := range staleIDs {
			_, _ = tx.ExecContext(ctx, `DELETE FROM assets WHERE id = ?`, id)
		}
		if err := tx.Commit(); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return nil
}

func (idx *Index) indexOneFile(
	ctx context.Context,
	fc config.FolderConfig,
	sidecarRoot string,
	absPath string,
	relPosix string,
	info fs.FileInfo,
) error {
	assetID := MakeAssetID(fc.ID, relPosix)
	mtimeNs := info.ModTime().UTC().UnixNano()
	sizeBytes := info.Size()

	// Check if already indexed with matching mtime, size, and sidecar mtime.
	var sidecarMTimeNs int64
	if canPath, err := sidecar.CanonicalEditsPath(sidecarRoot, relPosix); err == nil {
		if sInfo, sErr := os.Stat(canPath); sErr == nil {
			sidecarMTimeNs = sInfo.ModTime().UTC().UnixNano()
		}
	}

	var curSize, curMTime, curSidecarMTime int64
	err := idx.db.QueryRowContext(ctx,
		`SELECT size_bytes, mtime_ns, sidecar_mtime_ns FROM assets WHERE id = ?`,
		assetID,
	).Scan(&curSize, &curMTime, &curSidecarMTime)
	if err == nil && curSize == sizeBytes && curMTime == mtimeNs && curSidecarMTime == sidecarMTimeNs {
		return nil
	}

	sourceRev := computeSourceRevision(absPath, sizeBytes, mtimeNs)
	width, height, hasGainMap := thumb.ProbeImageHeader(absPath)

	// Read sidecar (following legacy read precedence; quarantines corrupt canonical .edits).
	var (
		rating   int
		flag     string
		label    string
		keywords []string
		edited   bool
		editedAt string
		hash     string
	)
	if res, rerr := idx.sidecars.Read(fc.Path, sidecarRoot, relPosix); rerr == nil && res.Sidecar != nil {
		rating, flag, label, keywords, edited = sidecar.ExtractVerdicts(res.Sidecar)
		if h, ok := res.Sidecar.Asset["hash"].(string); ok {
			hash = h
		}
		if latest := sidecar.LatestHLC(res.Sidecar); latest != "" {
			if p, perr := hlc.Parse(latest); perr == nil {
				editedAt = time.UnixMilli(int64(p.Millis)).UTC().Format(time.RFC3339Nano)
			}
		}
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	capturedAt := info.ModTime().UTC().Format(time.RFC3339Nano)
	gainInt := 0
	if hasGainMap {
		gainInt = 1
	}
	editedInt := 0
	if edited {
		editedInt = 1
	}

	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()

	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO assets(
			id, folder_id, rel_path, name, kind, mime_type, size_bytes, mtime_ns,
			content_hash, source_revision, width, height, orientation, has_gain_map,
			captured_at, imported_at, edited_at, rating, flag, color_label,
			edited_by_hdrtoys, exif_json, sidecar_mtime_ns
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?)
		ON CONFLICT(id) DO UPDATE SET
			size_bytes = excluded.size_bytes,
			mtime_ns = excluded.mtime_ns,
			content_hash = CASE WHEN excluded.content_hash != '' THEN excluded.content_hash ELSE assets.content_hash END,
			source_revision = excluded.source_revision,
			width = excluded.width,
			height = excluded.height,
			has_gain_map = excluded.has_gain_map,
			edited_at = excluded.edited_at,
			rating = excluded.rating,
			flag = excluded.flag,
			color_label = excluded.color_label,
			edited_by_hdrtoys = excluded.edited_by_hdrtoys,
			sidecar_mtime_ns = excluded.sidecar_mtime_ns`,
		assetID, fc.ID, relPosix, filepath.Base(absPath), formats.Kind(absPath), formats.MIME(absPath),
		sizeBytes, mtimeNs, hash, sourceRev, width, height, gainInt,
		capturedAt, nowStr, editedAt, rating, flag, label, editedInt, sidecarMTimeNs,
	); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_keywords WHERE asset_id = ?`, assetID); err != nil {
		return err
	}
	for _, kw := range keywords {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO asset_keywords(asset_id, keyword) VALUES(?, ?)`,
			assetID, kw,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// StartWatcher watches configured folders and their .hdrtoys/ sidecar trees via
// fsnotify (with debounced rescan) so external edits appear automatically.
func (idx *Index) StartWatcher(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	addTree := func(root string) {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil //nolint:nilerr // best-effort directory watcher registration
			}
			base := d.Name()
			if base == ".tmp" || base == ".quarantine" || strings.HasPrefix(base, "._") {
				return filepath.SkipDir
			}
			_ = watcher.Add(p)
			return nil
		})
	}

	for _, fc := range idx.Folders() {
		addTree(fc.Path)
		sRoot := sidecar.SidecarRoot(fc.Path, fc.ID, idx.sidecarDir)
		if info, statErr := os.Stat(sRoot); statErr == nil && info.IsDir() {
			addTree(sRoot)
		}
	}

	go func() {
		defer func() { _ = watcher.Close() }()
		var timer *time.Timer
		triggerRescan := func() {
			if timer != nil {
				timer.Reset(350 * time.Millisecond)
				return
			}
			timer = time.AfterFunc(350*time.Millisecond, func() {
				_ = idx.ScanAll(ctx)
			})
		}

		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Create) {
					if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
						addTree(ev.Name)
					}
				}
				if strings.Contains(ev.Name, string(filepath.Separator)+".tmp"+string(filepath.Separator)) {
					continue
				}
				triggerRescan()
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			}
		}
	}()
	return nil
}
