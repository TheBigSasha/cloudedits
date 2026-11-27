package index

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

// The change journal is the ops_log table read by GET /changes. Besides the
// ops POST /ops applied, it records what the server saw happen to an asset
// outside the op stream, so a client that follows the feed also learns about
// edits another program made directly in the folder.
const (
	outcomeApplied = "applied"
	// OutcomeObserved marks a feed entry that is not an op: the server
	// noticed the asset change. Clients re-read the asset instead of applying
	// a value.
	OutcomeObserved = "observed"

	// FieldSidecar is the field of an observed entry reporting that an
	// asset's .edits state changed outside POST /ops.
	FieldSidecar = "sidecar"
	// FieldEditThumbnail is the field of an observed entry reporting that an
	// asset's edited preview was replaced.
	FieldEditThumbnail = "editThumbnail"

	// HeadCursor is the since value that asks only for the journal head.
	HeadCursor = "head"

	// observedDevice is the device an observed entry reports.
	observedDevice = "cloudedits"
	// observedRowDevice keys observed rows in ops_log. No op can carry it:
	// an op's device is the device part of a well-formed HLC it also sends.
	observedRowDevice = "@observed"

	metaJournalEpoch = "epoch"
	metaJournalFloor = "floor"
)

// journalRetainRows is how many of the newest journal rows are kept. Older
// rows are removed; a client whose cursor points into the removed part is told
// to take a snapshot. The journal also answers "was this (device, hlc) already
// processed", so a retry that arrives after its row was removed is evaluated
// again and reported stale or applied instead of duplicate.
var journalRetainRows int64 = 20000

// journalPruneSlack is how far the journal may overshoot journalRetainRows
// before it is trimmed, so trimming is one bulk delete per many appends.
func journalPruneSlack() int64 {
	return max(journalRetainRows/8, 1)
}

func randomToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// loadJournalMeta reads the journal's identity, creating it for a new index.
// The epoch names this journal: a cursor issued by another index (the file
// was deleted and rebuilt) carries another epoch and can never be mistaken
// for a position here.
func (idx *Index) loadJournalMeta(ctx context.Context) error {
	if _, err := idx.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO journal_meta(key, value) VALUES(?, ?), (?, '0')`,
		metaJournalEpoch, randomToken(), metaJournalFloor,
	); err != nil {
		return fmt.Errorf("initialize journal: %w", err)
	}
	var epoch, floor string
	if err := idx.db.QueryRowContext(ctx, `SELECT value FROM journal_meta WHERE key = ?`, metaJournalEpoch).Scan(&epoch); err != nil {
		return fmt.Errorf("read journal epoch: %w", err)
	}
	if err := idx.db.QueryRowContext(ctx, `SELECT value FROM journal_meta WHERE key = ?`, metaJournalFloor).Scan(&floor); err != nil {
		return fmt.Errorf("read journal floor: %w", err)
	}
	n, err := strconv.ParseInt(floor, 10, 64)
	if err != nil {
		return fmt.Errorf("read journal floor: %w", err)
	}
	idx.mu.Lock()
	idx.journalEpoch = epoch
	idx.mu.Unlock()
	idx.journalFloor = n
	return nil
}

func (idx *Index) epoch() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.journalEpoch
}

// formatCursor renders a journal position as the opaque cursor clients hold.
func (idx *Index) formatCursor(seq int64) string {
	return idx.epoch() + "." + strconv.FormatInt(seq, 10)
}

// parseCursor returns the position a cursor of this journal names. ok is
// false for a cursor of another journal or one that is not a cursor at all.
func (idx *Index) parseCursor(cursor string) (seq int64, ok bool) {
	epoch, pos, found := strings.Cut(strings.TrimSpace(cursor), ".")
	if !found || epoch != idx.epoch() {
		return 0, false
	}
	n, err := strconv.ParseInt(pos, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// assetLock returns the mutex that makes one asset's sidecar write and the
// journal rows describing it a single step. POST /ops holds it from before it
// rewrites the sidecar until its rows are committed; the scanner only examines
// an asset while it can take the lock, so it never reads a sidecar whose ops
// are written but not yet journaled and mistakes them for an outside edit.
func (idx *Index) assetLock(id string) *sync.Mutex {
	idx.assetMuGuard.Lock()
	defer idx.assetMuGuard.Unlock()
	mu, ok := idx.assetMus[id]
	if !ok {
		mu = &sync.Mutex{}
		idx.assetMus[id] = mu
	}
	return mu
}

// feedState is what the journal last accounted for per asset.
type feedState struct {
	digest string // sidecar.EditDigest of the accounted edit state
	thumb  int64  // sidecar.EditThumbnailStamp of the accounted preview
	have   bool
}

func readFeedStateTx(ctx context.Context, tx *sql.Tx, assetID string) (feedState, error) {
	var st feedState
	err := tx.QueryRowContext(ctx,
		`SELECT sidecar_digest, thumb_stamp FROM asset_feed_state WHERE asset_id = ?`, assetID,
	).Scan(&st.digest, &st.thumb)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	st.have = err == nil
	return st, err
}

func writeFeedStateTx(ctx context.Context, tx *sql.Tx, assetID, digest string, thumb int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO asset_feed_state(asset_id, sidecar_digest, thumb_stamp) VALUES(?, ?, ?)
		ON CONFLICT(asset_id) DO UPDATE SET sidecar_digest = excluded.sidecar_digest, thumb_stamp = excluded.thumb_stamp`,
		assetID, digest, thumb)
	return err
}

// observedHLC is the HLC an observed sidecar entry reports: the newest one in
// the sidecar, so a client that folds it into its clock stamps its next edit
// after everything the other program wrote. A sidecar without one (it was
// removed) gets a tick of this server's clock.
func (idx *Index) observedHLC(latest string) string {
	if latest != "" && idx.clock.Observe(latest) == nil {
		return latest
	}
	return idx.clock.Tick()
}

// journalObservedTx appends an observed entry for assetID and returns its seq.
func journalObservedTx(ctx context.Context, tx *sql.Tx, assetID, field, hlcStr string) (int64, error) {
	raw, err := json.Marshal(sidecar.TruthOp{HLC: hlcStr, Device: observedDevice, AssetKey: assetID, Field: field})
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(raw)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO ops_log(device, hlc, asset_id, field, payload_hash, outcome, op_json) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		observedRowDevice, randomToken(), assetID, field, hex.EncodeToString(sum[:8]), OutcomeObserved, string(raw),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// pruneJournalTx trims the journal to its newest journalRetainRows rows once
// it has overshot by the slack. It returns the floor to adopt after commit.
// Caller holds writeMu.
func (idx *Index) pruneJournalTx(ctx context.Context, tx *sql.Tx, headSeq int64) (int64, error) {
	floor := idx.journalFloor
	if headSeq-floor <= journalRetainRows+journalPruneSlack() {
		return floor, nil
	}
	// seq values are not dense (an aborted transaction leaves a gap), so the
	// cut is the seq of the row that is journalRetainRows from the head.
	var cut sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT seq FROM ops_log ORDER BY seq DESC LIMIT 1 OFFSET ?`, journalRetainRows,
	).Scan(&cut); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return floor, err
	}
	if !cut.Valid || cut.Int64 <= floor {
		return floor, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ops_log WHERE seq <= ?`, cut.Int64); err != nil {
		return floor, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE journal_meta SET value = ? WHERE key = ?`, strconv.FormatInt(cut.Int64, 10), metaJournalFloor,
	); err != nil {
		return floor, err
	}
	return cut.Int64, nil
}

// journalBounds reads the retained range: floor is the last removed position
// (0 when nothing was removed) and head the last appended one.
func (idx *Index) journalBounds(ctx context.Context) (floor, head int64, err error) {
	var floorText string
	var maxSeq sql.NullInt64
	if err := idx.db.QueryRowContext(ctx, `
		SELECT (SELECT value FROM journal_meta WHERE key = ?), (SELECT MAX(seq) FROM ops_log)`,
		metaJournalFloor,
	).Scan(&floorText, &maxSeq); err != nil {
		return 0, 0, fmt.Errorf("read journal bounds: %w", err)
	}
	floor, err = strconv.ParseInt(floorText, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("read journal floor: %w", err)
	}
	return floor, max(maxSeq.Int64, floor), nil
}

// NoteEditThumbnail journals that asset's edited preview was replaced through
// the API, so other clients refresh the thumbnail they show.
func (idx *Index) NoteEditThumbnail(ctx context.Context, asset *Asset) error {
	mu := idx.assetLock(asset.ID)
	mu.Lock()
	defer mu.Unlock()

	stamp := sidecar.EditThumbnailStamp(asset.SidecarRoot, asset.RelPath)

	idx.writeMu.Lock()
	defer idx.writeMu.Unlock()
	tx, err := idx.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	st, err := readFeedStateTx(ctx, tx, asset.ID)
	if err != nil {
		return err
	}
	digest := st.digest
	if !st.have {
		// No baseline yet: take the sidecar as it is, so a later comparison
		// does not report the edits it already holds as an outside change.
		digest = idx.readSidecarView(asset.FolderRoot, asset.SidecarRoot, asset.RelPath).digest
	}
	seq, err := journalObservedTx(ctx, tx, asset.ID, FieldEditThumbnail, idx.clock.Tick())
	if err != nil {
		return err
	}
	if err := writeFeedStateTx(ctx, tx, asset.ID, digest, stamp); err != nil {
		return err
	}
	floor, err := idx.pruneJournalTx(ctx, tx, seq)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	idx.journalFloor = floor
	return nil
}
