package index

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

func names(list []Asset) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.Name
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFilterAcceptsEditorWireForms pins the list-query vocabulary the editor
// actually sends: v1 "<field>_<dir>" sort tokens, flag=any / label=any
// ("any flagged" / "any labeled"), and a repeatable kind parameter with the
// jpeg and photo aliases.
func TestFilterAcceptsEditorWireForms(t *testing.T) {
	ctx := context.Background()
	photoDir := t.TempDir()
	for _, n := range []string{"b.jpg", "a.jpeg", "c.png", "d.dng"} {
		writeTestJPEG(t, filepath.Join(photoDir, n))
	}
	folders := []config.FolderConfig{{ID: "p", Path: photoDir, Label: "P"}}
	idx, err := Open(filepath.Join(t.TempDir(), "index.db"), "", folders, sidecar.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	ops := []sidecar.TruthOp{
		{HLC: "019a2f001000-0001-d", Device: "d", AssetKey: MakeAssetID("p", "a.jpeg"), Field: "rating", Value: 2},
		{HLC: "019a2f001000-0002-d", Device: "d", AssetKey: MakeAssetID("p", "b.jpg"), Field: "rating", Value: 5},
		{HLC: "019a2f001000-0003-d", Device: "d", AssetKey: MakeAssetID("p", "b.jpg"), Field: "flag", Value: "reject"},
		{HLC: "019a2f001000-0004-d", Device: "d", AssetKey: MakeAssetID("p", "c.png"), Field: "colorLabel", Value: "red"},
		{HLC: "019a2f001000-0005-d", Device: "d", AssetKey: MakeAssetID("p", "c.png"), Field: "flag", Value: "pick"},
	}
	if _, err := idx.ApplyOpsBatch(ctx, ops); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		f    Filter
		want []string
	}{
		{"sort token name_asc", Filter{Sort: "name_asc"}, []string{"a.jpeg", "b.jpg", "c.png", "d.dng"}},
		{"sort token name_desc", Filter{Sort: "name_desc"}, []string{"d.dng", "c.png", "b.jpg", "a.jpeg"}},
		{"sort token rating_desc", Filter{Sort: "rating_desc", Kind: "jpeg"}, []string{"b.jpg", "a.jpeg"}},
		{"explicit dir wins over token", Filter{Sort: "name_asc", Dir: "desc"}, []string{"d.dng", "c.png", "b.jpg", "a.jpeg"}},
		{"flag any = any flagged", Filter{Flag: "any", Sort: "name", Dir: "asc"}, []string{"b.jpg", "c.png"}},
		{"label any = any labeled", Filter{Label: "any"}, []string{"c.png"}},
		{"kind jpeg covers .jpg and .jpeg", Filter{Kind: "jpeg", Sort: "name", Dir: "asc"}, []string{"a.jpeg", "b.jpg"}},
		{"kind photo = everything but raw", Filter{Kind: "photo", Sort: "name", Dir: "asc"}, []string{"a.jpeg", "b.jpg", "c.png"}},
		{"repeated kind is OR", Filter{Kind: "raw,png", Sort: "name", Dir: "asc"}, []string{"c.png", "d.dng"}},
	}
	for _, tc := range cases {
		list, total, err := idx.Range(ctx, tc.f, 0, 50)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := names(list); !equalStrings(got, tc.want) || total != len(tc.want) {
			t.Errorf("%s: got %v (total=%d), want %v", tc.name, got, total, tc.want)
		}
	}
}

// queryPlan returns SQLite's plan for the list query a filter produces.
func queryPlan(t *testing.T, idx *Index, f Filter) string {
	t.Helper()
	where, args := buildWhere(f)
	rows, err := idx.db.Query(`EXPLAIN QUERY PLAN SELECT `+assetColumns+` FROM assets a`+where+buildOrderBy(f)+` LIMIT 200 OFFSET 0`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return strings.Join(plan, " | ")
}

// TestEverySortOrderIsIndexBacked pins that no sort order the API exposes
// makes SQLite sort the whole table per request: an unfiltered window must
// walk an index in order. A full sort grows with the library and breaks the
// range-query latency budget well before 100,000 assets.
func TestEverySortOrderIsIndexBacked(t *testing.T) {
	idx, _ := openScanned(t, 3)
	for _, sort := range []string{"captured", "imported", "edited", "name", "rating"} {
		for _, dir := range []string{"asc", "desc"} {
			plan := queryPlan(t, idx, Filter{Sort: sort, Dir: dir})
			if !strings.Contains(plan, "USING INDEX") && !strings.Contains(plan, "USING COVERING INDEX") {
				t.Errorf("sort=%s dir=%s scans without an index: %s", sort, dir, plan)
			}
			if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
				t.Errorf("sort=%s dir=%s sorts the whole result per request: %s", sort, dir, plan)
			}
			if sort == "name" && strings.Contains(plan, "TEMP B-TREE") {
				t.Errorf("sort=name dir=%s needs a sorter although idx_assets_name covers it: %s", dir, plan)
			}
		}
	}
}
