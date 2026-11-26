package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// A folder that cannot be scanned does not stop the others; their errors are
// returned joined.
func (idx *Index) ScanAll(ctx context.Context) error {
	var errs []error
	for _, f := range idx.Folders() {
		if err := idx.ScanFolder(ctx, f); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// fileStamp is the cheap identity of an indexed file: when all three values
// match what the index holds, the file needs no work at all.
type fileStamp struct {
	sizeBytes      int64
	mtimeNs        int64
	sidecarMtimeNs int64
}

// sidecarView is what the index projects out of an asset's .edits sidecar.
type sidecarView struct {
	rating   int
	flag     string
	label    string
	keywords []string
	edited   bool
	editedAt string
	hash     string
}

// assetRow is one fully computed assets-table row waiting to be written.
type assetRow struct {
	id, relPosix, absPath string
	stamp                 fileStamp
	sourceRev             string
	width, height         int
	hasGainMap            bool
	capturedAt            string
	sidecar               sidecarView
}

// scanBatchSize bounds how many rows one write transaction carries, and so
// how long a scan holds the index write lock away from POST /ops.
const scanBatchSize = 64

// ScanFolder scans a single configured folder strictly read-only.
//
// Unchanged files cost one stat each: the folder's known stamps are loaded
// with a single query up front, so a rescan of an unchanged library issues no
// per-file statements. New and changed files are written in batches.
func (idx *Index) ScanFolder(ctx context.Context, fc config.FolderConfig) error {
	sidecarRoot := sidecar.SidecarRoot(fc.Path, fc.ID, idx.sidecarDir)

	// An unreadable root (unmounted drive or share) must not be mistaken for
	// an empty folder: that would prune every asset the folder holds.
	if info, err := os.Stat(fc.Path); err != nil {
		return fmt.Errorf("folder %q unavailable: %w", fc.ID, err)
	} else if !info.IsDir() {
		return fmt.Errorf("folder %q unavailable: %s is not a directory", fc.ID, fc.Path)
	}

	known, err := idx.knownStamps(ctx, fc.ID)
	if err != nil {
		return fmt.Errorf("load indexed files of folder %q: %w", fc.ID, err)
	}
	seen := make(map[string]struct{}, len(known))
	pending := make([]assetRow, 0, scanBatchSize)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		err := idx.upsertRows(ctx, fc, sidecarRoot, pending)
		pending = pending[:0]
		return err
	}

	err = filepath.WalkDir(fc.Path, func(p string, d fs.DirEntry, walkErr error) error {
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
		seen[assetID] = struct{}{}

		stamp := fileStamp{
			sizeBytes:      info.Size(),
			mtimeNs:        info.ModTime().UTC().UnixNano(),
			sidecarMtimeNs: sidecarMtimeNs(sidecarRoot, relPosix),
		}
		if cur, ok := known[assetID]; ok && cur == stamp {
			return nil
		}
		pending = append(pending, idx.buildRow(fc, sidecarRoot, assetID, p, relPosix, stamp, info))
		if len(pending) >= scanBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}

	// Prune assets belonging to this folder that no longer exist on disk.
	var staleIDs []string
	for id := range known {
		if _, ok := seen[id]; !ok {
			staleIDs = append(staleIDs, id)
		}
	}
	if len(staleIDs) == 0 {
		return nil
	}
	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range staleIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM assets WHERE id = ?`, id); err != nil {
			return fmt.Errorf("prune %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// knownStamps loads the stamp of every indexed file of one folder.
func (idx *Index) knownStamps(ctx context.Context, folderID string) (map[string]fileStamp, error) {
	rows, err := idx.db.QueryContext(ctx,
		`SELECT id, size_bytes, mtime_ns, sidecar_mtime_ns FROM assets WHERE folder_id = ?`, folderID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]fileStamp{}
	for rows.Next() {
		var (
			id string
			st fileStamp
		)
		if err := rows.Scan(&id, &st.sizeBytes, &st.mtimeNs, &st.sidecarMtimeNs); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// sidecarMtimeNs returns the canonical .edits modification time, or 0 when
// the asset has no sidecar.
func sidecarMtimeNs(sidecarRoot, relPosix string) int64 {
	canPath, err := sidecar.CanonicalEditsPath(sidecarRoot, relPosix)
	if err != nil {
		return 0
	}
	info, err := os.Stat(canPath)
	if err != nil {
		return 0
	}
	return info.ModTime().UTC().UnixNano()
}

// readSidecarView reads an asset's sidecar (following legacy read precedence;
// a corrupt canonical .edits is quarantined by the store) and projects the
// fields the index keeps.
func (idx *Index) readSidecarView(folderRoot, sidecarRoot, relPosix string) sidecarView {
	var v sidecarView
	res, err := idx.sidecars.Read(folderRoot, sidecarRoot, relPosix)
	if err != nil || res.Sidecar == nil {
		return v
	}
	v.rating, v.flag, v.label, v.keywords, v.edited = sidecar.ExtractVerdicts(res.Sidecar)
	if h, ok := res.Sidecar.Asset["hash"].(string); ok {
		v.hash = h
	}
	v.editedAt = editedAtOf(res.Sidecar)
	return v
}

// editedAtOf derives the edited-at timestamp from the newest HLC in sc, or ""
// when the sidecar carries none.
func editedAtOf(sc *sidecar.Sidecar) string {
	latest := sidecar.LatestHLC(sc)
	if latest == "" {
		return ""
	}
	p, err := hlc.Parse(latest)
	if err != nil {
		return ""
	}
	return time.UnixMilli(int64(p.Millis)).UTC().Format(time.RFC3339Nano)
}

// buildRow does all per-file work that needs no index lock: source revision,
// header probe, and the sidecar read.
func (idx *Index) buildRow(
	fc config.FolderConfig,
	sidecarRoot, assetID, absPath, relPosix string,
	stamp fileStamp,
	info fs.FileInfo,
) assetRow {
	width, height, hasGainMap := thumb.ProbeImageHeader(absPath)
	return assetRow{
		id:         assetID,
		relPosix:   relPosix,
		absPath:    absPath,
		stamp:      stamp,
		sourceRev:  computeSourceRevision(absPath, stamp.sizeBytes, stamp.mtimeNs),
		width:      width,
		height:     height,
		hasGainMap: hasGainMap,
		capturedAt: info.ModTime().UTC().Format(time.RFC3339Nano),
		sidecar:    idx.readSidecarView(fc.Path, sidecarRoot, relPosix),
	}
}

// upsertRows writes one batch of rows in a single transaction.
func (idx *Index) upsertRows(ctx context.Context, fc config.FolderConfig, sidecarRoot string, rows []assetRow) error {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)

	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()

	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for i := range rows {
		row := &rows[i]
		// The sidecar was read before this lock was taken. POST /ops writes
		// the .edits file first and projects it under the same lock, so a
		// sidecar that moved since the read is re-read here; otherwise this
		// batch could overwrite a newer projection with the older view.
		if now := sidecarMtimeNs(sidecarRoot, row.relPosix); now != row.stamp.sidecarMtimeNs {
			row.stamp.sidecarMtimeNs = now
			row.sidecar = idx.readSidecarView(fc.Path, sidecarRoot, row.relPosix)
		}
		sc := row.sidecar
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
			row.id, fc.ID, row.relPosix, filepath.Base(row.absPath), formats.Kind(row.absPath), formats.MIME(row.absPath),
			row.stamp.sizeBytes, row.stamp.mtimeNs, sc.hash, row.sourceRev, row.width, row.height, boolInt(row.hasGainMap),
			row.capturedAt, nowStr, sc.editedAt, sc.rating, sc.flag, sc.label, boolInt(sc.edited), row.stamp.sidecarMtimeNs,
		); err != nil {
			return fmt.Errorf("index %s: %w", row.relPosix, err)
		}
		if err := replaceKeywordsTx(ctx, tx, row.id, sc.keywords); err != nil {
			return fmt.Errorf("index keywords of %s: %w", row.relPosix, err)
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// replaceKeywordsTx makes keywords the exact keyword set of assetID.
func replaceKeywordsTx(ctx context.Context, tx *sql.Tx, assetID string, keywords []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_keywords WHERE asset_id = ?`, assetID); err != nil {
		return err
	}
	for _, kw := range keywords {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO asset_keywords(asset_id, keyword) VALUES(?, ?)`, assetID, kw,
		); err != nil {
			return err
		}
	}
	return nil
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
