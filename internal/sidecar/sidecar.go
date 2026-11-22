// Package sidecar implements the canonical .hdrtoys/<rel-posix-path>.edits
// sidecar format: parsing with unknown-field preservation, canonical key-sorted
// JSON serialization, HLC LWW + variant join-semilattice merging, conflict
// recipe rescue, atomic durability, legacy precedence, and corruption quarantine.
package sidecar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/thebigsasha/cloudedits/internal/hlc"
)

// HDRTVersion is the current sidecar schema version emitted by writers.
// Readers accept any hdrt >= 1 under the additive-only compatibility policy.
const HDRTVersion = 1

// FieldCell is a versioned field value stamped with an HLC.
type FieldCell struct {
	V     any
	HLC   string
	Extra map[string]any
}

// Variant is an append-only creative snapshot record keyed by ID.
type Variant struct {
	ID        string
	HLC       string
	Name      *string
	Recipe    any
	Tombstone bool
	Extra     map[string]any
}

// DocumentMeta holds the self-describing document role ("truth" or "portable").
type DocumentMeta struct {
	Role  string
	Extra map[string]any
}

// Sidecar represents a parsed .edits document with full unknown-field preservation.
type Sidecar struct {
	HDRT       int
	Document   *DocumentMeta
	Asset      map[string]any
	Fields     map[string]FieldCell
	Recipe     *FieldCell
	Variants   []Variant
	Provenance map[string]any
	Extra      map[string]any
}

// TruthOp represents one journaled HLC-stamped mutation from POST /ops.
type TruthOp struct {
	HLC       string `json:"hlc"`
	Device    string `json:"device"`
	AssetKey  string `json:"assetKey"`
	File      string `json:"file,omitempty"`
	Field     string `json:"field"`
	Value     any    `json:"value"`
	UndoGroup string `json:"undoGroup,omitempty"`
}

// MergeOptions configures Merge behavior.
type MergeOptions struct {
	// RescueConflictingRecipes preserves the losing recipe as an immutable
	// manual variant ("Merged edit") whenever two truth sidecars carry distinct
	// recipes.
	RescueConflictingRecipes bool
}

// Empty returns a fresh empty truth sidecar for an asset.
func Empty(file string, sizeBytes *int64, contentHash string) *Sidecar {
	asset := map[string]any{}
	if file != "" {
		asset["file"] = file
	}
	if sizeBytes != nil {
		asset["bytes"] = json.Number(fmt.Sprintf("%d", *sizeBytes))
	}
	if contentHash != "" {
		asset["hash"] = contentHash
	}
	return &Sidecar{
		HDRT:     HDRTVersion,
		Document: &DocumentMeta{Role: "truth"},
		Asset:    asset,
		Fields:   map[string]FieldCell{},
		Extra:    map[string]any{},
	}
}

var knownTopKeys = map[string]bool{
	"hdrt":       true,
	"document":   true,
	"asset":      true,
	"fields":     true,
	"recipe":     true,
	"variants":   true,
	"provenance": true,
}

// Parse decodes a .edits JSON document without dropping unknown fields.
func Parse(data []byte) (*Sidecar, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("sidecar root is not an object")
	}

	hdrtNum, err := parsePositiveInt(obj["hdrt"])
	if err != nil {
		return nil, fmt.Errorf("missing/invalid hdrt version: %w", err)
	}

	var doc *DocumentMeta
	if rawDoc, exists := obj["document"]; exists && rawDoc != nil {
		docMap, ok := rawDoc.(map[string]any)
		if !ok {
			return nil, errors.New("document is not an object")
		}
		role, _ := docMap["role"].(string)
		if role != "truth" && role != "portable" {
			return nil, errors.New("document.role must be truth or portable")
		}
		extra := map[string]any{}
		for k, v := range docMap {
			if k != "role" {
				extra[k] = v
			}
		}
		doc = &DocumentMeta{Role: role, Extra: extra}
	}

	rawAsset, exists := obj["asset"]
	if !exists {
		return nil, errors.New("missing asset block")
	}
	assetMap, ok := rawAsset.(map[string]any)
	if !ok {
		return nil, errors.New("asset is not an object")
	}

	fields := map[string]FieldCell{}
	if rawFields, exists := obj["fields"]; exists && rawFields != nil {
		fm, ok := rawFields.(map[string]any)
		if !ok {
			return nil, errors.New("fields is not an object")
		}
		for k, v := range fm {
			cell, err := parseFieldCell(v)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", k, err)
			}
			fields[k] = cell
		}
	}

	var recipe *FieldCell
	if rawRecipe, exists := obj["recipe"]; exists && rawRecipe != nil {
		cell, err := parseFieldCell(rawRecipe)
		if err != nil {
			return nil, fmt.Errorf("recipe: %w", err)
		}
		recipe = &cell
	}

	var variants []Variant
	if rawVariants, exists := obj["variants"]; exists && rawVariants != nil {
		arr, ok := rawVariants.([]any)
		if !ok {
			return nil, errors.New("variants is not an array")
		}
		variants = make([]Variant, 0, len(arr))
		for i, item := range arr {
			vr, err := parseVariant(item)
			if err != nil {
				return nil, fmt.Errorf("variants[%d]: %w", i, err)
			}
			variants = append(variants, vr)
		}
	}

	var provenance map[string]any
	if rawProv, exists := obj["provenance"]; exists && rawProv != nil {
		pm, ok := rawProv.(map[string]any)
		if !ok {
			return nil, errors.New("provenance is not an object")
		}
		provenance = cloneMap(pm)
	}

	extra := map[string]any{}
	for k, v := range obj {
		if !knownTopKeys[k] {
			extra[k] = v
		}
	}

	return &Sidecar{
		HDRT:       hdrtNum,
		Document:   doc,
		Asset:      cloneMap(assetMap),
		Fields:     fields,
		Recipe:     recipe,
		Variants:   variants,
		Provenance: provenance,
		Extra:      extra,
	}, nil
}

func parsePositiveInt(v any) (int, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 1 {
			return 0, errors.New("must be integer >= 1")
		}
		return int(i), nil
	case float64:
		if n < 1 || n != float64(int(n)) {
			return 0, errors.New("must be integer >= 1")
		}
		return int(n), nil
	case int:
		if n < 1 {
			return 0, errors.New("must be integer >= 1")
		}
		return n, nil
	default:
		return 0, errors.New("not a number")
	}
}

func parseFieldCell(v any) (FieldCell, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return FieldCell{}, errors.New("cell is not an object")
	}
	h, ok := m["hlc"].(string)
	if !ok || h == "" {
		return FieldCell{}, errors.New("missing hlc")
	}
	if _, err := hlc.Parse(h); err != nil {
		return FieldCell{}, err
	}
	extra := map[string]any{}
	for k, val := range m {
		if k != "v" && k != "hlc" {
			extra[k] = val
		}
	}
	return FieldCell{
		V:     m["v"],
		HLC:   h,
		Extra: extra,
	}, nil
}

func parseVariant(v any) (Variant, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Variant{}, errors.New("variant is not an object")
	}
	id, ok := m["id"].(string)
	if !ok || id == "" {
		return Variant{}, errors.New("variant missing id")
	}
	h, ok := m["hlc"].(string)
	if !ok || h == "" {
		return Variant{}, errors.New("variant missing hlc")
	}
	if _, err := hlc.Parse(h); err != nil {
		return Variant{}, err
	}
	vr := Variant{
		ID:    id,
		HLC:   h,
		Extra: map[string]any{},
	}
	if rawName, exists := m["name"]; exists && rawName != nil {
		if s, ok := rawName.(string); ok {
			vr.Name = &s
		}
	}
	if rawRecipe, exists := m["recipe"]; exists {
		vr.Recipe = rawRecipe
	}
	if rawTomb, exists := m["tombstone"]; exists {
		if b, ok := rawTomb.(bool); ok {
			vr.Tombstone = b
		}
	}
	for k, val := range m {
		switch k {
		case "id", "hlc", "name", "recipe", "tombstone":
		default:
			vr.Extra[k] = val
		}
	}
	return vr, nil
}

func fieldCellToMap(c FieldCell) map[string]any {
	m := map[string]any{
		"hlc": c.HLC,
		"v":   c.V,
	}
	maps.Copy(m, c.Extra)
	return m
}

func variantToMap(v Variant, includeTombstone bool) map[string]any {
	m := map[string]any{
		"id":  v.ID,
		"hlc": v.HLC,
	}
	if v.Name != nil {
		m["name"] = *v.Name
	}
	if v.Recipe != nil {
		m["recipe"] = v.Recipe
	}
	if includeTombstone && v.Tombstone {
		m["tombstone"] = true
	}
	maps.Copy(m, v.Extra)
	return m
}

// Serialize emits canonical JSON: "hdrt" first, all other keys recursively
// sorted lexicographically, variants sorted by id, 2-space indent, trailing "\n".
func Serialize(s *Sidecar) ([]byte, error) {
	if s == nil {
		return nil, errors.New("nil sidecar")
	}
	ver := max(s.HDRT, HDRTVersion)

	top := map[string]any{}
	maps.Copy(top, s.Extra)
	if s.Document != nil {
		docMap := map[string]any{"role": s.Document.Role}
		maps.Copy(docMap, s.Document.Extra)
		top["document"] = docMap
	}
	if s.Asset != nil {
		top["asset"] = s.Asset
	} else {
		top["asset"] = map[string]any{}
	}
	fieldsMap := make(map[string]any, len(s.Fields))
	for k, cell := range s.Fields {
		fieldsMap[k] = fieldCellToMap(cell)
	}
	top["fields"] = fieldsMap
	if s.Recipe != nil {
		top["recipe"] = fieldCellToMap(*s.Recipe)
	}
	if len(s.Variants) > 0 {
		sorted := slices.Clone(s.Variants)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
		arr := make([]any, len(sorted))
		for i, v := range sorted {
			arr[i] = variantToMap(v, true)
		}
		top["variants"] = arr
	}
	if s.Provenance != nil {
		top["provenance"] = s.Provenance
	}

	keys := make([]string, 0, len(top))
	for k := range top {
		if k != "hdrt" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "{\n  \"hdrt\": %d", ver)
	for _, k := range keys {
		buf.WriteString(",\n  ")
		if err := writeJSONString(&buf, k); err != nil {
			return nil, err
		}
		buf.WriteString(": ")
		if err := writeCanonicalValue(&buf, top[k], 1, true); err != nil {
			return nil, err
		}
	}
	buf.WriteString("\n}\n")
	return buf.Bytes(), nil
}

// CanonicalCompactJSON returns compact recursively key-sorted JSON bytes.
func CanonicalCompactJSON(v any) (string, error) {
	var buf bytes.Buffer
	if err := writeCanonicalValue(&buf, v, 0, false); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func writeJSONString(buf *bytes.Buffer, s string) error {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	b := bytes.TrimSuffix(tmp.Bytes(), []byte("\n"))
	buf.Write(b)
	return nil
}

func writeIndent(buf *bytes.Buffer, depth int) {
	for i := 0; i < depth; i++ {
		buf.WriteString("  ")
	}
}

func writeCanonicalValue(buf *bytes.Buffer, v any, depth int, pretty bool) error {
	switch val := v.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		return writeJSONString(buf, val)
	case json.Number:
		buf.WriteString(val.String())
		return nil
	case int, int64, int32, uint, uint64, uint32, float64, float32:
		b, err := json.Marshal(val)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	case []string:
		if len(val) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteByte('[')
		for i, elem := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if pretty {
				buf.WriteByte('\n')
				writeIndent(buf, depth+1)
			}
			if err := writeJSONString(buf, elem); err != nil {
				return err
			}
		}
		if pretty {
			buf.WriteByte('\n')
			writeIndent(buf, depth)
		}
		buf.WriteByte(']')
		return nil
	case []any:
		if len(val) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteByte('[')
		for i, elem := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if pretty {
				buf.WriteByte('\n')
				writeIndent(buf, depth+1)
			}
			if err := writeCanonicalValue(buf, elem, depth+1, pretty); err != nil {
				return err
			}
		}
		if pretty {
			buf.WriteByte('\n')
			writeIndent(buf, depth)
		}
		buf.WriteByte(']')
		return nil
	case map[string]any:
		if len(val) == 0 {
			buf.WriteString("{}")
			return nil
		}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if pretty {
				buf.WriteByte('\n')
				writeIndent(buf, depth+1)
			}
			if err := writeJSONString(buf, k); err != nil {
				return err
			}
			if pretty {
				buf.WriteString(": ")
			} else {
				buf.WriteByte(':')
			}
			if err := writeCanonicalValue(buf, val[k], depth+1, pretty); err != nil {
				return err
			}
		}
		if pretty {
			buf.WriteByte('\n')
			writeIndent(buf, depth)
		}
		buf.WriteByte('}')
		return nil
	default:
		// Normalize any typed struct/map/slice through json with UseNumber.
		raw, err := json.Marshal(val)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var normalized any
		if err := dec.Decode(&normalized); err != nil {
			return err
		}
		return writeCanonicalValue(buf, normalized, depth, pretty)
	}
}

// FieldWins returns true iff incoming strictly beats existing under the total
// local order: numeric HLC comparison, with ties broken by canonical JSON bytes.
func FieldWins(incoming, existing FieldCell) (bool, error) {
	cmp, err := hlc.Compare(incoming.HLC, existing.HLC)
	if err != nil {
		return false, err
	}
	if cmp != 0 {
		return cmp > 0, nil
	}
	incBytes, err := CanonicalCompactJSON(fieldCellToMap(incoming))
	if err != nil {
		return false, err
	}
	exBytes, err := CanonicalCompactJSON(fieldCellToMap(existing))
	if err != nil {
		return false, err
	}
	return incBytes > exBytes, nil
}

// ApplyOp mutates s in place according to the LWW + variant-semilattice rules.
// Returns changed=true if s was modified.
func ApplyOp(s *Sidecar, op TruthOp) (bool, error) {
	if _, err := hlc.Parse(op.HLC); err != nil {
		return false, err
	}
	if s.Fields == nil {
		s.Fields = map[string]FieldCell{}
	}
	switch op.Field {
	case "import":
		return false, nil
	case "recipe":
		incoming := FieldCell{V: op.Value, HLC: op.HLC}
		if s.Recipe != nil {
			wins, err := FieldWins(incoming, *s.Recipe)
			if err != nil {
				return false, err
			}
			if !wins {
				return false, nil
			}
		}
		s.Recipe = &incoming
		return true, nil
	case "variant":
		vr, err := variantFromOpValue(op.Value, op.HLC)
		if err != nil {
			return false, err
		}
		return joinVariant(&s.Variants, vr)
	default:
		incoming := FieldCell{V: op.Value, HLC: op.HLC}
		if cur, exists := s.Fields[op.Field]; exists {
			wins, err := FieldWins(incoming, cur)
			if err != nil {
				return false, err
			}
			if !wins {
				return false, nil
			}
		}
		s.Fields[op.Field] = incoming
		return true, nil
	}
}

func variantFromOpValue(v any, opHLC string) (Variant, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Variant{}, errors.New("variant op value must be an object")
	}
	id, ok := m["id"].(string)
	if !ok || id == "" {
		return Variant{}, errors.New("variant op value missing id")
	}
	vr := Variant{
		ID:    id,
		HLC:   opHLC,
		Extra: map[string]any{},
	}
	if rawName, exists := m["name"]; exists && rawName != nil {
		if s, ok := rawName.(string); ok {
			vr.Name = &s
		}
	}
	if rawRecipe, exists := m["recipe"]; exists {
		vr.Recipe = rawRecipe
	}
	if rawTomb, exists := m["tombstone"]; exists {
		if b, ok := rawTomb.(bool); ok {
			vr.Tombstone = b
		}
	}
	for k, val := range m {
		switch k {
		case "id", "hlc", "name", "recipe", "tombstone":
		default:
			vr.Extra[k] = val
		}
	}
	return vr, nil
}

// JoinVariants computes the join-semilattice of two records with the same ID.
// Payload selects max by (HLC, canonical payload without tombstone);
// Tombstone is logical OR so deletion dominates forever regardless of HLC.
func JoinVariants(existing, incoming Variant) (Variant, error) {
	tomb := existing.Tombstone || incoming.Tombstone
	cmp, err := hlc.Compare(incoming.HLC, existing.HLC)
	if err != nil {
		return Variant{}, err
	}
	winner := existing
	if cmp > 0 {
		winner = incoming
	} else if cmp == 0 {
		incBytes, err := CanonicalCompactJSON(variantToMap(incoming, false))
		if err != nil {
			return Variant{}, err
		}
		exBytes, err := CanonicalCompactJSON(variantToMap(existing, false))
		if err != nil {
			return Variant{}, err
		}
		if incBytes > exBytes {
			winner = incoming
		}
	}
	joined := cloneVariant(winner)
	joined.Tombstone = tomb
	return joined, nil
}

func joinVariant(list *[]Variant, incoming Variant) (bool, error) {
	for i := range *list {
		if (*list)[i].ID == incoming.ID {
			joined, err := JoinVariants((*list)[i], incoming)
			if err != nil {
				return false, err
			}
			oldBytes, err := CanonicalCompactJSON(variantToMap((*list)[i], true))
			if err != nil {
				return false, err
			}
			newBytes, err := CanonicalCompactJSON(variantToMap(joined, true))
			if err != nil {
				return false, err
			}
			if oldBytes == newBytes {
				return false, nil
			}
			(*list)[i] = joined
			return true, nil
		}
	}
	*list = append(*list, cloneVariant(incoming))
	return true, nil
}

// ConflictRescueVariant constructs the deterministic immutable manual variant
// preserving a losing recipe during concurrent merge.
func ConflictRescueVariant(loser FieldCell) (Variant, error) {
	compact, err := CanonicalCompactJSON(loser.V)
	if err != nil {
		return Variant{}, err
	}
	sum := sha256.Sum256([]byte(compact))
	shortHash := hex.EncodeToString(sum[:4])
	name := "Merged edit"
	return Variant{
		ID:     fmt.Sprintf("conflict-%s-%s", loser.HLC, shortHash),
		HLC:    loser.HLC,
		Name:   &name,
		Recipe: loser.V,
		Extra:  map[string]any{"snapshotKind": "manual"},
	}, nil
}

// Merge joins two copies of the same asset's sidecar deterministically.
func Merge(a, b *Sidecar, opts MergeOptions) (*Sidecar, error) {
	if a == nil {
		return cloneSidecar(b), nil
	}
	if b == nil {
		return cloneSidecar(a), nil
	}
	roleA := "truth"
	if a.Document != nil && a.Document.Role != "" {
		roleA = a.Document.Role
	}
	roleB := "truth"
	if b.Document != nil && b.Document.Role != "" {
		roleB = b.Document.Role
	}
	if roleA == "portable" && roleB == "truth" {
		return cloneSidecar(b), nil
	}
	if roleA == "truth" && roleB == "portable" {
		return cloneSidecar(a), nil
	}

	docExtra, err := joinValueMaps(docExtraMap(a.Document), docExtraMap(b.Document))
	if err != nil {
		return nil, err
	}
	role := "portable"
	if roleA == "truth" || roleB == "truth" {
		role = "truth"
	}

	asset, err := joinValueMaps(a.Asset, b.Asset)
	if err != nil {
		return nil, err
	}
	extra, err := joinValueMaps(a.Extra, b.Extra)
	if err != nil {
		return nil, err
	}
	var prov map[string]any
	if a.Provenance != nil || b.Provenance != nil {
		p, err := joinValues(a.Provenance, b.Provenance)
		if err != nil {
			return nil, err
		}
		if pm, ok := p.(map[string]any); ok {
			prov = pm
		}
	}

	out := &Sidecar{
		HDRT:       max(a.HDRT, b.HDRT, HDRTVersion),
		Document:   &DocumentMeta{Role: role, Extra: docExtra},
		Asset:      asset,
		Fields:     map[string]FieldCell{},
		Provenance: prov,
		Extra:      extra,
	}

	for _, src := range []*Sidecar{a, b} {
		for k, cell := range src.Fields {
			if cur, exists := out.Fields[k]; !exists {
				out.Fields[k] = cloneFieldCell(cell)
			} else {
				wins, err := FieldWins(cell, cur)
				if err != nil {
					return nil, err
				}
				if wins {
					out.Fields[k] = cloneFieldCell(cell)
				}
			}
		}
	}

	var rescued *Variant
	switch {
	case a.Recipe != nil && b.Recipe != nil:
		aWins, err := FieldWins(*a.Recipe, *b.Recipe)
		if err != nil {
			return nil, err
		}
		bWins, err := FieldWins(*b.Recipe, *a.Recipe)
		if err != nil {
			return nil, err
		}
		var winner, loser FieldCell
		switch {
		case aWins:
			winner = cloneFieldCell(*a.Recipe)
			loser = cloneFieldCell(*b.Recipe)
		case bWins:
			winner = cloneFieldCell(*b.Recipe)
			loser = cloneFieldCell(*a.Recipe)
		default:
			winner = cloneFieldCell(*a.Recipe)
			loser = cloneFieldCell(*b.Recipe)
		}
		out.Recipe = &winner

		if opts.RescueConflictingRecipes && winner.V != nil && loser.V != nil {
			wBytes, err := CanonicalCompactJSON(winner.V)
			if err != nil {
				return nil, err
			}
			lBytes, err := CanonicalCompactJSON(loser.V)
			if err != nil {
				return nil, err
			}
			if wBytes != lBytes {
				rv, err := ConflictRescueVariant(loser)
				if err != nil {
					return nil, err
				}
				rescued = &rv
			}
		}
	case a.Recipe != nil:
		c := cloneFieldCell(*a.Recipe)
		out.Recipe = &c
	case b.Recipe != nil:
		c := cloneFieldCell(*b.Recipe)
		out.Recipe = &c
	}

	var joined []Variant
	for _, src := range []*Sidecar{a, b} {
		for _, vr := range src.Variants {
			if _, err := joinVariant(&joined, vr); err != nil {
				return nil, err
			}
		}
	}
	if rescued != nil {
		if _, err := joinVariant(&joined, *rescued); err != nil {
			return nil, err
		}
	}
	if len(joined) > 0 {
		sort.Slice(joined, func(i, j int) bool { return joined[i].ID < joined[j].ID })
		out.Variants = joined
	}

	return out, nil
}

// LatestHLC returns the greatest HLC across fields, recipe, and variants.
func LatestHLC(s *Sidecar) string {
	if s == nil {
		return ""
	}
	best := ""
	consider := func(h string) {
		if h == "" {
			return
		}
		if best == "" {
			best = h
			return
		}
		if cmp, err := hlc.Compare(h, best); err == nil && cmp > 0 {
			best = h
		}
	}
	for _, cell := range s.Fields {
		consider(cell.HLC)
	}
	if s.Recipe != nil {
		consider(s.Recipe.HLC)
	}
	for _, vr := range s.Variants {
		consider(vr.HLC)
	}
	return best
}

func docExtraMap(d *DocumentMeta) map[string]any {
	if d == nil {
		return nil
	}
	return d.Extra
}

func joinValues(a, b any) (any, error) {
	if a == nil {
		return b, nil
	}
	if b == nil {
		return a, nil
	}
	ab, err := CanonicalCompactJSON(a)
	if err != nil {
		return nil, err
	}
	bb, err := CanonicalCompactJSON(b)
	if err != nil {
		return nil, err
	}
	if bb > ab {
		return b, nil
	}
	return a, nil
}

func joinValueMaps(a, b map[string]any) (map[string]any, error) {
	out := map[string]any{}
	maps.Copy(out, a)
	for k, bv := range b {
		if av, exists := out[k]; exists {
			joined, err := joinValues(av, bv)
			if err != nil {
				return nil, err
			}
			out[k] = joined
		} else {
			out[k] = bv
		}
	}
	return out, nil
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	maps.Copy(out, m)
	return out
}

func cloneFieldCell(c FieldCell) FieldCell {
	return FieldCell{
		V:     c.V,
		HLC:   c.HLC,
		Extra: cloneMap(c.Extra),
	}
}

func cloneVariant(v Variant) Variant {
	out := Variant{
		ID:        v.ID,
		HLC:       v.HLC,
		Recipe:    v.Recipe,
		Tombstone: v.Tombstone,
		Extra:     cloneMap(v.Extra),
	}
	if v.Name != nil {
		s := *v.Name
		out.Name = &s
	}
	return out
}

func cloneSidecar(s *Sidecar) *Sidecar {
	if s == nil {
		return nil
	}
	out := &Sidecar{
		HDRT:       s.HDRT,
		Asset:      cloneMap(s.Asset),
		Fields:     make(map[string]FieldCell, len(s.Fields)),
		Provenance: cloneMap(s.Provenance),
		Extra:      cloneMap(s.Extra),
	}
	if s.Document != nil {
		out.Document = &DocumentMeta{Role: s.Document.Role, Extra: cloneMap(s.Document.Extra)}
	}
	for k, c := range s.Fields {
		out.Fields[k] = cloneFieldCell(c)
	}
	if s.Recipe != nil {
		c := cloneFieldCell(*s.Recipe)
		out.Recipe = &c
	}
	if len(s.Variants) > 0 {
		out.Variants = make([]Variant, len(s.Variants))
		for i, v := range s.Variants {
			out.Variants[i] = cloneVariant(v)
		}
	}
	return out
}

// ExtractVerdicts pulls typed rating/flag/colorLabel/keywords/editedByHdrtoys
// summaries from a sidecar for SQLite index projection.
func ExtractVerdicts(s *Sidecar) (rating int, flag, colorLabel string, keywords []string, edited bool) {
	if s == nil {
		return 0, "", "", nil, false
	}
	if c, ok := s.Fields["rating"]; ok {
		if r, err := parseNonNegInt(c.V); err == nil {
			rating = r
		}
	}
	if c, ok := s.Fields["flag"]; ok {
		if str, ok := c.V.(string); ok {
			flag = str
		}
	}
	if c, ok := s.Fields["colorLabel"]; ok {
		if str, ok := c.V.(string); ok {
			colorLabel = str
		}
	}
	if c, ok := s.Fields["keywords"]; ok {
		keywords = extractStringSlice(c.V)
	}
	if s.Recipe != nil && s.Recipe.V != nil {
		edited = true
	}
	return rating, flag, colorLabel, keywords, edited
}

func parseNonNegInt(v any) (int, error) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0, errors.New("invalid int")
		}
		return int(i), nil
	case float64:
		if n < 0 {
			return 0, errors.New("negative")
		}
		return int(n), nil
	case int:
		if n < 0 {
			return 0, errors.New("negative")
		}
		return n, nil
	default:
		return 0, errors.New("not a number")
	}
}

func extractStringSlice(v any) []string {
	switch arr := v.(type) {
	case []string:
		return slices.Clone(arr)
	case []any:
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
