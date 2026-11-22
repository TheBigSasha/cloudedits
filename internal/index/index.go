// Package index maintains the strictly rebuildable SQLite WAL index and
// read-only folder scanner + fsnotify watcher for cloudedits.
// Deleting index.db and restarting reconstructs 100% of library state losslessly
// from source files and .hdrtoys/**/*.edits sidecars.
package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	// Register pure-Go SQLite database/sql driver.
	_ "modernc.org/sqlite"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/formats"
	"github.com/thebigsasha/cloudedits/internal/hlc"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

// Asset represents one indexed photo or RAW file.
type Asset struct {
	ID              string   `json:"id"`
	FolderID        string   `json:"folderId"`
	RelPath         string   `json:"relPath"`
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	MIME            string   `json:"mimeType"`
	SizeBytes       int64    `json:"sizeBytes"`
	MTimeMs         int64    `json:"mtimeMs"`
	ContentHash     string   `json:"contentHash,omitempty"`
	SourceRevision  string   `json:"sourceRevision"`
	Width           int      `json:"width,omitempty"`
	Height          int      `json:"height,omitempty"`
	Orientation     int      `json:"orientation,omitempty"`
	HasGainMap      bool     `json:"hasGainMap"`
	CapturedAt      string   `json:"capturedAt"`
	ImportedAt      string   `json:"importedAt"`
	EditedAt        string   `json:"editedAt,omitempty"`
	Rating          int      `json:"rating"`
	Flag            *string  `json:"flag"`
	ColorLabel      *string  `json:"colorLabel"`
	Keywords        []string `json:"keywords"`
	EditedByHdrtoys bool     `json:"editedByHdrtoys"`
	EXIFJSON        string   `json:"-"`
	FolderRoot      string   `json:"-"`
	SidecarRoot     string   `json:"-"`
	AbsPath         string   `json:"-"`
}

// GridAsset is the compact projection returned for fields=grid range queries.
type GridAsset struct {
	ID              string   `json:"id"`
	FolderID        string   `json:"folderId"`
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	MIME            string   `json:"mimeType"`
	SourceRevision  string   `json:"sourceRevision"`
	Width           int      `json:"width,omitempty"`
	Height          int      `json:"height,omitempty"`
	HasGainMap      bool     `json:"hasGainMap"`
	CapturedAt      string   `json:"capturedAt"`
	Rating          int      `json:"rating"`
	Flag            *string  `json:"flag"`
	ColorLabel      *string  `json:"colorLabel"`
	Keywords        []string `json:"keywords,omitempty"`
	EditedByHdrtoys bool     `json:"editedByHdrtoys"`
}

// ToGrid converts an Asset to its compact grid representation.
func (a Asset) ToGrid() GridAsset {
	return GridAsset{
		ID:              a.ID,
		FolderID:        a.FolderID,
		Name:            a.Name,
		Kind:            a.Kind,
		MIME:            a.MIME,
		SourceRevision:  a.SourceRevision,
		Width:           a.Width,
		Height:          a.Height,
		HasGainMap:      a.HasGainMap,
		CapturedAt:      a.CapturedAt,
		Rating:          a.Rating,
		Flag:            a.Flag,
		ColorLabel:      a.ColorLabel,
		Keywords:        a.Keywords,
		EditedByHdrtoys: a.EditedByHdrtoys,
	}
}

// Filter specifies filtering and sorting parameters for asset queries.
type Filter struct {
	Query     string
	Rating    *int
	RatingGte *int
	Flag      string
	Label     string
	Keywords  []string
	Kind      string
	Folder    string
	Sort      string // captured | imported | edited | name | rating
	Dir       string // asc | desc
}

// OpReceipt reports the durable outcome of one op in POST /ops.
type OpReceipt struct {
	Device         string `json:"device"`
	HLC            string `json:"hlc"`
	Status         string `json:"status"` // "applied" | "duplicate" | "stale"
	OriginalStatus string `json:"originalStatus,omitempty"`
}

// ChangeOp represents one journaled op returned by GET /changes.
type ChangeOp struct {
	Seq     int64           `json:"seq"`
	Device  string          `json:"device"`
	HLC     string          `json:"hlc"`
	AssetID string          `json:"assetId"`
	Field   string          `json:"field"`
	Outcome string          `json:"outcome"`
	Op      json.RawMessage `json:"op"`
}

// ChangesFeed is returned by GET /changes?since=<cursor>.
type ChangesFeed struct {
	Ops        []ChangeOp `json:"ops"`
	Assets     []Asset    `json:"assets"`
	NextCursor string     `json:"nextCursor"`
}

// Index owns the SQLite WAL database, configured folders, and sidecar store.
type Index struct {
	dbPath     string
	db         *sql.DB
	sidecars   *sidecar.Store
	sidecarDir string

	writeMu sync.Mutex
	mu      sync.RWMutex
	folders map[string]config.FolderConfig
}

// MakeAssetID constructs the opaque URL-safe asset ID for a (folderID, relPosix) pair.
func MakeAssetID(folderID, relPosix string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(folderID + "/" + relPosix))
}

// Open initializes or opens the rebuildable SQLite WAL index at dbPath.
func Open(dbPath, customSidecarDir string, folders []config.FolderConfig, sidecars *sidecar.Store) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create index dir: %w", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}

	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	if sidecars == nil {
		sidecars = sidecar.NewStore()
	}

	idx := &Index{
		dbPath:     dbPath,
		db:         db,
		sidecars:   sidecars,
		sidecarDir: customSidecarDir,
		folders:    map[string]config.FolderConfig{},
	}
	if err := idx.SetFolders(context.Background(), folders); err != nil {
		_ = db.Close()
		return nil, err
	}
	return idx, nil
}

// Close closes the underlying SQLite connection.
func (idx *Index) Close() error {
	if idx == nil || idx.db == nil {
		return nil
	}
	return idx.db.Close()
}

// Sidecars returns the associated atomic sidecar store.
func (idx *Index) Sidecars() *sidecar.Store {
	return idx.sidecars
}

// SidecarDir returns the optional external sidecar directory (empty means default inside-folder .hdrtoys/).
func (idx *Index) SidecarDir() string {
	return idx.sidecarDir
}

// SidecarRootFor returns the resolved writable sidecar root for folderID.
func (idx *Index) SidecarRootFor(folderID string) (string, error) {
	idx.mu.RLock()
	fc, ok := idx.folders[folderID]
	idx.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown folder %q", folderID)
	}
	return sidecar.SidecarRoot(fc.Path, fc.ID, idx.sidecarDir), nil
}

// Folders returns a sorted snapshot of configured folders.
func (idx *Index) Folders() []config.FolderConfig {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make([]config.FolderConfig, 0, len(idx.folders))
	for _, f := range idx.folders {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SetFolders updates the active folder registry in memory and SQLite.
func (idx *Index) SetFolders(ctx context.Context, folders []config.FolderConfig) error {
	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()

	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM folders`); err != nil {
		return err
	}
	next := make(map[string]config.FolderConfig, len(folders))
	for _, f := range folders {
		abs, err := filepath.Abs(f.Path)
		if err != nil {
			return err
		}
		f.Path = abs
		sRoot := sidecar.SidecarRoot(f.Path, f.ID, idx.sidecarDir)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO folders(folder_id, root_path, label, sidecar_dir) VALUES(?, ?, ?, ?)`,
			f.ID, f.Path, f.Label, sRoot,
		); err != nil {
			return err
		}
		next[f.ID] = f
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	idx.mu.Lock()
	idx.folders = next
	idx.mu.Unlock()
	return nil
}

func initSchema(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS folders (
	folder_id TEXT PRIMARY KEY,
	root_path TEXT NOT NULL,
	label TEXT NOT NULL,
	sidecar_dir TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS assets (
	id TEXT PRIMARY KEY,
	folder_id TEXT NOT NULL,
	rel_path TEXT NOT NULL,
	name TEXT NOT NULL,
	kind TEXT NOT NULL,
	mime_type TEXT NOT NULL,
	size_bytes INTEGER NOT NULL,
	mtime_ns INTEGER NOT NULL,
	content_hash TEXT NOT NULL DEFAULT '',
	source_revision TEXT NOT NULL,
	width INTEGER NOT NULL DEFAULT 0,
	height INTEGER NOT NULL DEFAULT 0,
	orientation INTEGER NOT NULL DEFAULT 1,
	has_gain_map INTEGER NOT NULL DEFAULT 0,
	captured_at TEXT NOT NULL,
	imported_at TEXT NOT NULL,
	edited_at TEXT NOT NULL DEFAULT '',
	rating INTEGER NOT NULL DEFAULT 0,
	flag TEXT NOT NULL DEFAULT '',
	color_label TEXT NOT NULL DEFAULT '',
	edited_by_hdrtoys INTEGER NOT NULL DEFAULT 0,
	exif_json TEXT NOT NULL DEFAULT '{}',
	sidecar_mtime_ns INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS asset_keywords (
	asset_id TEXT NOT NULL,
	keyword TEXT NOT NULL,
	PRIMARY KEY(asset_id, keyword),
	FOREIGN KEY(asset_id) REFERENCES assets(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS ops_log (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	device TEXT NOT NULL,
	hlc TEXT NOT NULL,
	asset_id TEXT NOT NULL,
	field TEXT NOT NULL,
	payload_hash TEXT NOT NULL,
	outcome TEXT NOT NULL,
	op_json TEXT NOT NULL,
	UNIQUE(device, hlc)
);

CREATE INDEX IF NOT EXISTS idx_assets_captured ON assets(captured_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_assets_rating ON assets(rating, captured_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_assets_flag ON assets(flag, captured_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_assets_label ON assets(color_label, captured_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_assets_edited ON assets(edited_at DESC, id);
CREATE INDEX IF NOT EXISTS idx_assets_folder_rel ON assets(folder_id, rel_path);
CREATE INDEX IF NOT EXISTS idx_keywords_kw ON asset_keywords(keyword, asset_id);
`
	_, err := db.Exec(ddl)
	return err
}

func buildWhere(f Filter) (clause string, args []any) {
	var conds []string
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		conds = append(conds, `(LOWER(a.name) LIKE ? OR LOWER(a.rel_path) LIKE ? OR EXISTS (SELECT 1 FROM asset_keywords ak WHERE ak.asset_id = a.id AND LOWER(ak.keyword) LIKE ?))`)
		args = append(args, like, like, like)
	}
	if f.Rating != nil {
		conds = append(conds, `a.rating = ?`)
		args = append(args, *f.Rating)
	}
	if f.RatingGte != nil {
		conds = append(conds, `a.rating >= ?`)
		args = append(args, *f.RatingGte)
	}
	if fl := strings.TrimSpace(f.Flag); fl != "" {
		if fl == "none" {
			conds = append(conds, `a.flag = ''`)
		} else {
			conds = append(conds, `a.flag = ?`)
			args = append(args, fl)
		}
	}
	if lb := strings.TrimSpace(f.Label); lb != "" {
		if lb == "none" {
			conds = append(conds, `a.color_label = ''`)
		} else {
			conds = append(conds, `a.color_label = ?`)
			args = append(args, lb)
		}
	}
	for _, kw := range f.Keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		conds = append(conds, `EXISTS (SELECT 1 FROM asset_keywords ak WHERE ak.asset_id = a.id AND LOWER(ak.keyword) = LOWER(?))`)
		args = append(args, kw)
	}
	if k := strings.ToLower(strings.TrimSpace(f.Kind)); k != "" {
		if k == "raw" || k == "image" {
			conds = append(conds, `a.kind = ?`)
			args = append(args, k)
		} else {
			ext := k
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			conds = append(conds, `LOWER(a.name) LIKE ?`)
			args = append(args, "%"+ext)
		}
	}
	if fl := strings.TrimSpace(f.Folder); fl != "" {
		conds = append(conds, `(a.folder_id = ? OR EXISTS (SELECT 1 FROM folders fo WHERE fo.folder_id = a.folder_id AND fo.label = ?))`)
		args = append(args, fl, fl)
	}

	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func buildOrderBy(f Filter) string {
	dir := "DESC"
	if strings.EqualFold(f.Dir, "asc") {
		dir = "ASC"
	}
	switch strings.ToLower(strings.TrimSpace(f.Sort)) {
	case "imported":
		return fmt.Sprintf(" ORDER BY a.imported_at %s, a.id ASC", dir)
	case "edited":
		return fmt.Sprintf(" ORDER BY a.edited_at %s, a.id ASC", dir)
	case "name":
		return fmt.Sprintf(" ORDER BY LOWER(a.name) %s, a.id ASC", dir)
	case "rating":
		return fmt.Sprintf(" ORDER BY a.rating %s, a.captured_at DESC, a.id ASC", dir)
	default:
		return fmt.Sprintf(" ORDER BY a.captured_at %s, a.id ASC", dir)
	}
}

// Count returns the number of assets matching f.
func (idx *Index) Count(ctx context.Context, f Filter) (int, error) {
	where, args := buildWhere(f)
	var total int
	err := idx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assets a`+where, args...).Scan(&total)
	return total, err
}

// Range returns a slice of assets at [offset, offset+limit) matching f, along with total count.
func (idx *Index) Range(ctx context.Context, f Filter, offset, limit int) ([]Asset, int, error) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	total, err := idx.Count(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	where, args := buildWhere(f)
	order := buildOrderBy(f)
	q := `SELECT a.id, a.folder_id, a.rel_path, a.name, a.kind, a.mime_type, a.size_bytes,
	             a.mtime_ns, a.content_hash, a.source_revision, a.width, a.height, a.orientation,
	             a.has_gain_map, a.captured_at, a.imported_at, a.edited_at, a.rating, a.flag,
	             a.color_label, a.edited_by_hdrtoys, a.exif_json
	      FROM assets a` + where + order + ` LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := idx.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	assets, err := idx.scanAssets(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return assets, total, nil
}

// ListCursor implements opaque cursor pagination over Range (limit <= 200).
func (idx *Index) ListCursor(ctx context.Context, f Filter, cursor string, limit int) (items []Asset, nextCursor string, total int, err error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	offset := 0
	if cursor != "" {
		raw, decErr := base64.RawURLEncoding.DecodeString(cursor)
		if decErr == nil {
			if n, pErr := strconv.Atoi(string(raw)); pErr == nil && n >= 0 {
				offset = n
			}
		}
	}
	items, total, err = idx.Range(ctx, f, offset, limit)
	if err != nil {
		return nil, "", 0, err
	}
	if offset+len(items) < total {
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset + len(items))))
	}
	return items, nextCursor, total, nil
}

// GetAsset fetches one asset by its opaque ID.
func (idx *Index) GetAsset(ctx context.Context, id string) (*Asset, error) {
	rows, err := idx.db.QueryContext(ctx, `
		SELECT a.id, a.folder_id, a.rel_path, a.name, a.kind, a.mime_type, a.size_bytes,
		       a.mtime_ns, a.content_hash, a.source_revision, a.width, a.height, a.orientation,
		       a.has_gain_map, a.captured_at, a.imported_at, a.edited_at, a.rating, a.flag,
		       a.color_label, a.edited_by_hdrtoys, a.exif_json
		FROM assets a WHERE a.id = ? LIMIT 1`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	list, err := idx.scanAssets(ctx, rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, os.ErrNotExist
	}
	return &list[0], nil
}

// ResolveAsset locates an asset by opaque ID, content_hash, or relative path / filename hint.
func (idx *Index) ResolveAsset(ctx context.Context, assetKey, fileHint string) (*Asset, error) {
	if assetKey != "" {
		if a, err := idx.GetAsset(ctx, assetKey); err == nil {
			return a, nil
		}
		var id string
		err := idx.db.QueryRowContext(ctx, `SELECT id FROM assets WHERE content_hash = ? LIMIT 1`, assetKey).Scan(&id)
		if err == nil && id != "" {
			return idx.GetAsset(ctx, id)
		}
		err = idx.db.QueryRowContext(ctx, `SELECT id FROM assets WHERE rel_path = ? OR name = ? LIMIT 1`, assetKey, assetKey).Scan(&id)
		if err == nil && id != "" {
			return idx.GetAsset(ctx, id)
		}
	}
	if fileHint != "" {
		var id string
		err := idx.db.QueryRowContext(ctx, `SELECT id FROM assets WHERE rel_path = ? OR name = ? LIMIT 1`, fileHint, fileHint).Scan(&id)
		if err == nil && id != "" {
			return idx.GetAsset(ctx, id)
		}
	}
	return nil, os.ErrNotExist
}

func (idx *Index) scanAssets(ctx context.Context, rows *sql.Rows) ([]Asset, error) {
	var out []Asset
	for rows.Next() {
		var (
			a        Asset
			mtimeNs  int64
			gainMap  int
			edited   int
			flagStr  string
			labelStr string
		)
		if err := rows.Scan(
			&a.ID, &a.FolderID, &a.RelPath, &a.Name, &a.Kind, &a.MIME, &a.SizeBytes,
			&mtimeNs, &a.ContentHash, &a.SourceRevision, &a.Width, &a.Height, &a.Orientation,
			&gainMap, &a.CapturedAt, &a.ImportedAt, &a.EditedAt, &a.Rating, &flagStr,
			&labelStr, &edited, &a.EXIFJSON,
		); err != nil {
			return nil, err
		}
		a.MTimeMs = mtimeNs / int64(time.Millisecond)
		a.HasGainMap = gainMap != 0
		a.EditedByHdrtoys = edited != 0
		if flagStr != "" {
			v := flagStr
			a.Flag = &v
		}
		if labelStr != "" {
			v := labelStr
			a.ColorLabel = &v
		}
		a.Keywords = []string{}
		idx.attachPaths(&a)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(out) == 0 {
		return []Asset{}, nil
	}

	// Batch load keywords for all returned assets.
	idSet := make([]string, len(out))
	byID := make(map[string]int, len(out))
	placeholders := make([]string, len(out))
	args := make([]any, len(out))
	for i, a := range out {
		idSet[i] = a.ID
		byID[a.ID] = i
		placeholders[i] = "?"
		args[i] = a.ID
	}
	kRows, err := idx.db.QueryContext(ctx,
		`SELECT asset_id, keyword FROM asset_keywords WHERE asset_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY keyword ASC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = kRows.Close() }()
	for kRows.Next() {
		var aid, kw string
		if err := kRows.Scan(&aid, &kw); err != nil {
			return nil, err
		}
		if pos, ok := byID[aid]; ok {
			out[pos].Keywords = append(out[pos].Keywords, kw)
		}
	}
	return out, kRows.Err()
}

func (idx *Index) attachPaths(a *Asset) {
	idx.mu.RLock()
	fc, ok := idx.folders[a.FolderID]
	idx.mu.RUnlock()
	if !ok {
		return
	}
	a.FolderRoot = fc.Path
	a.SidecarRoot = sidecar.SidecarRoot(fc.Path, fc.ID, idx.sidecarDir)
	if abs, err := sidecar.SafeJoinRel(fc.Path, a.RelPath); err == nil {
		a.AbsPath = abs
	}
}

// ApplyOpsBatch validates and commits a batch of HLC-stamped TruthOps with
// strict (device, hlc) idempotency, atomic .hdrtoys/<rel>.edits writes, and
// immediate SQLite projection updates.
func (idx *Index) ApplyOpsBatch(ctx context.Context, ops []sidecar.TruthOp) ([]OpReceipt, error) {
	if len(ops) == 0 {
		return []OpReceipt{}, nil
	}
	// Pre-validate every op in the batch before mutating anything.
	for i, op := range ops {
		if strings.TrimSpace(op.Device) == "" {
			return nil, fmt.Errorf("ops[%d]: missing device", i)
		}
		if _, err := hlc.Parse(op.HLC); err != nil {
			return nil, fmt.Errorf("ops[%d]: %w", i, err)
		}
		if strings.TrimSpace(op.Field) == "" {
			return nil, fmt.Errorf("ops[%d]: missing field", i)
		}
		if op.Field != "import" && strings.TrimSpace(op.AssetKey) == "" && strings.TrimSpace(op.File) == "" {
			return nil, fmt.Errorf("ops[%d]: missing assetKey", i)
		}
	}

	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()

	receipts := make([]OpReceipt, 0, len(ops))

	for _, op := range ops {
		// 1. Check (device, hlc) idempotency in ops_log.
		var existingOutcome string
		err := idx.db.QueryRowContext(ctx,
			`SELECT outcome FROM ops_log WHERE device = ? AND hlc = ?`,
			op.Device, op.HLC,
		).Scan(&existingOutcome)
		if err == nil {
			receipts = append(receipts, OpReceipt{
				Device:         op.Device,
				HLC:            op.HLC,
				Status:         "duplicate",
				OriginalStatus: existingOutcome,
			})
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		// Library-scope import marker: journal in ops_log only.
		if op.Field == "import" {
			if err := idx.recordOpLog(ctx, op, "@library", "applied"); err != nil {
				return nil, err
			}
			receipts = append(receipts, OpReceipt{Device: op.Device, HLC: op.HLC, Status: "applied"})
			continue
		}

		asset, err := idx.ResolveAsset(ctx, op.AssetKey, op.File)
		if err != nil {
			return nil, fmt.Errorf("resolve asset %q: %w", op.AssetKey, err)
		}

		sizeCopy := asset.SizeBytes
		sc, changed, err := idx.sidecars.Update(
			asset.FolderRoot,
			asset.SidecarRoot,
			asset.RelPath,
			&sizeCopy,
			asset.ContentHash,
			func(cur *sidecar.Sidecar) (bool, error) {
				return sidecar.ApplyOp(cur, op)
			},
		)
		if err != nil {
			return nil, fmt.Errorf("update sidecar for %s: %w", asset.ID, err)
		}

		outcome := "stale"
		if changed {
			outcome = "applied"
			if err := idx.syncAssetFromSidecarLocked(ctx, asset.ID, sc); err != nil {
				return nil, err
			}
		}

		if err := idx.recordOpLog(ctx, op, asset.ID, outcome); err != nil {
			return nil, err
		}

		receipts = append(receipts, OpReceipt{
			Device: op.Device,
			HLC:    op.HLC,
			Status: outcome,
		})
	}

	return receipts, nil
}

func (idx *Index) recordOpLog(ctx context.Context, op sidecar.TruthOp, assetID, outcome string) error {
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:8])
	_, err = idx.db.ExecContext(ctx,
		`INSERT INTO ops_log(device, hlc, asset_id, field, payload_hash, outcome, op_json) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		op.Device, op.HLC, assetID, op.Field, hash, outcome, string(raw),
	)
	return err
}

func (idx *Index) syncAssetFromSidecarLocked(ctx context.Context, assetID string, sc *sidecar.Sidecar) error {
	rating, flag, label, keywords, edited := sidecar.ExtractVerdicts(sc)
	editedInt := 0
	editedAt := ""
	if edited {
		editedInt = 1
	}
	if latest := sidecar.LatestHLC(sc); latest != "" {
		if p, err := hlc.Parse(latest); err == nil {
			editedAt = time.UnixMilli(int64(p.Millis)).UTC().Format(time.RFC3339Nano)
		}
	}

	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		UPDATE assets
		SET rating = ?, flag = ?, color_label = ?, edited_by_hdrtoys = ?,
		    edited_at = CASE WHEN ? != '' THEN ? ELSE edited_at END
		WHERE id = ?`,
		rating, flag, label, editedInt, editedAt, editedAt, assetID,
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

// ChangesSince returns all journaled ops after sinceCursor and the current state
// of affected assets for multi-device convergence.
func (idx *Index) ChangesSince(ctx context.Context, sinceCursor string, limit int) (*ChangesFeed, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var sinceSeq int64
	if sinceCursor != "" {
		if n, err := strconv.ParseInt(sinceCursor, 10, 64); err == nil && n >= 0 {
			sinceSeq = n
		}
	}

	rows, err := idx.db.QueryContext(ctx, `
		SELECT seq, device, hlc, asset_id, field, outcome, op_json
		FROM ops_log
		WHERE seq > ?
		ORDER BY seq ASC
		LIMIT ?`, sinceSeq, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ops := []ChangeOp{}
	assetIDs := map[string]bool{}
	lastSeq := sinceSeq
	for rows.Next() {
		var (
			c      ChangeOp
			opText string
		)
		if err := rows.Scan(&c.Seq, &c.Device, &c.HLC, &c.AssetID, &c.Field, &c.Outcome, &opText); err != nil {
			return nil, err
		}
		c.Op = json.RawMessage(opText)
		ops = append(ops, c)
		if c.AssetID != "" && c.AssetID != "@library" {
			assetIDs[c.AssetID] = true
		}
		if c.Seq > lastSeq {
			lastSeq = c.Seq
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	assets := make([]Asset, 0, len(assetIDs))
	for aid := range assetIDs {
		if a, err := idx.GetAsset(ctx, aid); err == nil {
			assets = append(assets, *a)
		}
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].ID < assets[j].ID })

	return &ChangesFeed{
		Ops:        ops,
		Assets:     assets,
		NextCursor: strconv.FormatInt(lastSeq, 10),
	}, nil
}

// WALSizeBytes returns the current SQLite WAL file size in bytes (0 if absent).
func (idx *Index) WALSizeBytes() int64 {
	if idx == nil || idx.dbPath == "" {
		return 0
	}
	info, err := os.Stat(idx.dbPath + "-wal")
	if err != nil {
		return 0
	}
	return info.Size()
}

// ResetIndex clears indexed rows and ops_log and rescans configured folders.
// Source files are never modified.
func (idx *Index) ResetIndex(ctx context.Context) error {
	idx.writeMu.Lock()
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		idx.writeMu.Unlock()
		return err
	}
	_, err1 := tx.ExecContext(ctx, `DELETE FROM asset_keywords`)
	_, err2 := tx.ExecContext(ctx, `DELETE FROM assets`)
	_, err3 := tx.ExecContext(ctx, `DELETE FROM ops_log`)
	if err1 != nil || err2 != nil || err3 != nil {
		_ = tx.Rollback()
		idx.writeMu.Unlock()
		return errors.Join(err1, err2, err3)
	}
	if err := tx.Commit(); err != nil {
		idx.writeMu.Unlock()
		return err
	}
	idx.writeMu.Unlock()

	return idx.ScanAll(ctx)
}

func computeSourceRevision(filePath string, size, mtimeNs int64) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d:%d:", size, mtimeNs)
	if f, err := os.Open(filePath); err == nil {
		var buf [4096]byte
		n, _ := io.ReadFull(f, buf[:])
		if n > 0 {
			h.Write(buf[:n])
		}
		_ = f.Close()
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func isSupported(name string) bool {
	return formats.IsSupportedFile(name)
}
