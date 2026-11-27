package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

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

// fileStamp is the cheap identity of an indexed file: when all four values
// match what the index holds, the file needs no work at all.
type fileStamp struct {
	sizeBytes      int64
	mtimeNs        int64
	sidecarMtimeNs int64
	// thumbStamp is sidecar.EditThumbnailStamp: the asset's edited preview.
	thumbStamp int64
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
	// digest is sidecar.EditDigest of the state read; latestHLC its newest HLC.
	digest    string
	latestHLC string
}

// assetRow is one fully computed assets-table row waiting to be written.
type assetRow struct {
	id, relPosix, absPath string
	stamp                 fileStamp
	sourceRev             string
	width, height         int
	orientation           int
	hasGainMap            bool
	capturedAt            string
	exifJSON              string
	sidecar               sidecarView
	// known is true when the asset was indexed before this scan; prev is the
	// stamp the index held for it.
	known bool
	prev  fileStamp
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
	thumbs := sidecar.EditThumbnailStamps(sidecarRoot)
	seen := make(map[string]struct{}, len(known))
	pending := make([]assetRow, 0, scanBatchSize)
	var busy []assetRow
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		skipped, err := idx.upsertRows(ctx, fc, sidecarRoot, pending)
		busy = append(busy, skipped...)
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
			sidecarMtimeNs: sidecar.ChangeStamp(fc.Path, sidecarRoot, relPosix),
			thumbStamp:     thumbs[relPosix],
		}
		cur, indexed := known[assetID]
		if indexed && cur == stamp {
			return nil
		}
		row := idx.buildRow(fc, sidecarRoot, assetID, p, relPosix, stamp, info)
		row.known, row.prev = indexed, cur
		pending = append(pending, row)
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
	// An asset whose ops were being committed was left alone. Its row still
	// carries the old stamp, so a later scan would pick it up anyway; a few
	// short retries settle it within this one.
	for attempt := 0; len(busy) > 0 && attempt < scanBusyRetries; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(scanBusyRetryDelay):
		}
		if busy, err = idx.upsertRows(ctx, fc, sidecarRoot, busy); err != nil {
			return err
		}
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
		if _, err := tx.ExecContext(ctx, `DELETE FROM asset_feed_state WHERE asset_id = ?`, id); err != nil {
			return fmt.Errorf("prune %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// Pacing of the retries for assets a scan found busy (see ScanFolder).
const (
	scanBusyRetries    = 5
	scanBusyRetryDelay = 20 * time.Millisecond
)

// knownStamps loads the stamp of every indexed file of one folder.
func (idx *Index) knownStamps(ctx context.Context, folderID string) (map[string]fileStamp, error) {
	rows, err := idx.db.QueryContext(ctx, `
		SELECT a.id, a.size_bytes, a.mtime_ns, a.sidecar_mtime_ns, COALESCE(s.thumb_stamp, 0)
		FROM assets a LEFT JOIN asset_feed_state s ON s.asset_id = a.id
		WHERE a.folder_id = ?`, folderID)
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
		if err := rows.Scan(&id, &st.sizeBytes, &st.mtimeNs, &st.sidecarMtimeNs, &st.thumbStamp); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
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
	v.digest = sidecar.EditDigest(res.Sidecar)
	v.latestHLC = sidecar.LatestHLC(res.Sidecar)
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
	exifMeta := thumb.ProbeEXIF(absPath)
	capturedAt := exifMeta.CapturedAt
	if capturedAt == "" {
		capturedAt = info.ModTime().UTC().Format(time.RFC3339Nano)
	}
	orientation := exifMeta.Orientation
	if orientation < 1 || orientation > 8 {
		orientation = 1
	}
	exifJSON := "{}"
	if len(exifMeta.Fields) > 0 {
		if b, err := json.Marshal(exifMeta.Fields); err == nil {
			exifJSON = string(b)
		}
	}
	return assetRow{
		id:          assetID,
		relPosix:    relPosix,
		absPath:     absPath,
		stamp:       stamp,
		sourceRev:   computeSourceRevision(absPath, stamp.sizeBytes, stamp.mtimeNs),
		width:       width,
		height:      height,
		orientation: orientation,
		hasGainMap:  hasGainMap,
		capturedAt:  capturedAt,
		exifJSON:    exifJSON,
		sidecar:     idx.readSidecarView(fc.Path, sidecarRoot, relPosix),
	}
}

// upsertRows writes one batch of rows in a single transaction and journals
// what the batch shows was changed outside the op stream. Rows of assets whose
// ops are being committed right now are returned untouched in skipped.
func (idx *Index) upsertRows(ctx context.Context, fc config.FolderConfig, sidecarRoot string, rows []assetRow) (skipped []assetRow, err error) {
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)

	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()

	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		headSeq int64
		held    []*sync.Mutex
	)
	defer func() {
		for _, mu := range held {
			mu.Unlock()
		}
	}()
	for i := range rows {
		row := &rows[i]
		// POST /ops holds the asset's lock from before it rewrites the
		// sidecar until its journal rows are committed, and it commits under
		// writeMu, which this scan holds. A lock that cannot be taken means a
		// rewrite may already be on disk with its ops not yet journaled.
		mu := idx.assetLock(row.id)
		if !mu.TryLock() {
			skipped = append(skipped, *row)
			continue
		}
		held = append(held, mu)

		// The sidecar was read before the locks were taken; one that moved
		// since is re-read, so this batch never writes an older view over a
		// newer projection.
		if now := sidecar.ChangeStamp(fc.Path, sidecarRoot, row.relPosix); now != row.stamp.sidecarMtimeNs {
			row.stamp.sidecarMtimeNs = now
			row.sidecar = idx.readSidecarView(fc.Path, sidecarRoot, row.relPosix)
		}
		row.stamp.thumbStamp = sidecar.EditThumbnailStamp(sidecarRoot, row.relPosix)
		sc := row.sidecar
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO assets(
				id, folder_id, rel_path, name, kind, mime_type, size_bytes, mtime_ns,
				content_hash, source_revision, width, height, orientation, has_gain_map,
				captured_at, imported_at, edited_at, rating, flag, color_label,
				edited_by_hdrtoys, exif_json, sidecar_mtime_ns
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				kind = excluded.kind,
				size_bytes = excluded.size_bytes,
				mtime_ns = excluded.mtime_ns,
				content_hash = CASE WHEN excluded.content_hash != '' THEN excluded.content_hash ELSE assets.content_hash END,
				source_revision = excluded.source_revision,
				width = excluded.width,
				height = excluded.height,
				orientation = excluded.orientation,
				has_gain_map = excluded.has_gain_map,
				captured_at = excluded.captured_at,
				edited_at = excluded.edited_at,
				rating = excluded.rating,
				flag = excluded.flag,
				color_label = excluded.color_label,
				edited_by_hdrtoys = excluded.edited_by_hdrtoys,
				exif_json = excluded.exif_json,
				sidecar_mtime_ns = excluded.sidecar_mtime_ns`,
			row.id, fc.ID, row.relPosix, filepath.Base(row.absPath), formats.AssetKind(row.absPath, row.hasGainMap), formats.MIME(row.absPath),
			row.stamp.sizeBytes, row.stamp.mtimeNs, sc.hash, row.sourceRev, row.width, row.height, row.orientation, boolInt(row.hasGainMap),
			row.capturedAt, nowStr, sc.editedAt, sc.rating, sc.flag, sc.label, boolInt(sc.edited), row.exifJSON, row.stamp.sidecarMtimeNs,
		); err != nil {
			return nil, fmt.Errorf("index %s: %w", row.relPosix, err)
		}
		if err := replaceKeywordsTx(ctx, tx, row.id, sc.keywords); err != nil {
			return nil, fmt.Errorf("index keywords of %s: %w", row.relPosix, err)
		}

		seq, err := idx.journalOutsideChangesTx(ctx, tx, row)
		if err != nil {
			return nil, fmt.Errorf("journal %s: %w", row.relPosix, err)
		}
		headSeq = max(headSeq, seq)
	}

	floor := idx.journalFloor
	if headSeq > 0 {
		if floor, err = idx.pruneJournalTx(ctx, tx, headSeq); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	idx.journalFloor = floor
	return skipped, nil
}

// journalOutsideChangesTx compares what the scan found for one asset with
// what the journal last accounted for, appends an observed entry for an edit
// state or edited preview that another program changed, and records the new
// state. It returns the seq of the last entry appended (0 for none).
//
// An asset new to the index only gets its baseline: clients learn of it by
// listing, and journaling every sidecar of a freshly built index would flood
// the feed with entries that describe no change.
func (idx *Index) journalOutsideChangesTx(ctx context.Context, tx *sql.Tx, row *assetRow) (int64, error) {
	st, err := readFeedStateTx(ctx, tx, row.id)
	if err != nil {
		return 0, err
	}
	sc := row.sidecar
	var lastSeq int64
	if row.known {
		// Without a baseline (an index from before the journal kept one) a
		// moved sidecar stamp is the evidence that the edits changed.
		sidecarChanged := st.have && st.digest != sc.digest ||
			!st.have && row.prev.sidecarMtimeNs != row.stamp.sidecarMtimeNs && sc.digest != ""
		if sidecarChanged {
			if lastSeq, err = journalObservedTx(ctx, tx, row.id, FieldSidecar, idx.observedHLC(sc.latestHLC)); err != nil {
				return 0, err
			}
		}
		if st.have && st.thumb != row.stamp.thumbStamp {
			if lastSeq, err = journalObservedTx(ctx, tx, row.id, FieldEditThumbnail, idx.clock.Tick()); err != nil {
				return 0, err
			}
		}
	}
	if !st.have || st.digest != sc.digest || st.thumb != row.stamp.thumbStamp {
		if err := writeFeedStateTx(ctx, tx, row.id, sc.digest, row.stamp.thumbStamp); err != nil {
			return 0, err
		}
	}
	return lastSeq, nil
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
