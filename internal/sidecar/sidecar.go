// Package sidecar implements the canonical .hdrtoys/<rel-posix-path>.edits
// sidecar format: parsing with unknown-field preservation, canonical key-sorted
// JSON serialization, HLC LWW + variant join-semilattice merging, conflict
// recipe rescue, atomic durability, legacy precedence, and corruption quarantine.
package sidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/thebigsasha/cloudedits/internal/hlc"
	"github.com/thebigsasha/cloudedits/internal/jsjson"
)

// HDRTVersion is the current sidecar schema version emitted by writers.
// Readers accept any hdrt >= 1 under the additive-only compatibility policy.
const HDRTVersion = 1

// FieldCell is a versioned field value stamped with an HLC.
type FieldCell struct {
	V     any
	HLC   string
	Extra map[string]any
	// NoValue records that the parsed cell carried no "v" member at all, so a
	// rewrite re-emits the cell without one instead of inventing "v": null.
	NoValue bool
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

	// provenanceOther preserves a "provenance" member that is present but not a
	// JSON object (including null) so a rewrite re-emits it verbatim.
	provenanceOther    any
	hasProvenanceOther bool
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

// Origin names the on-disk layout a sidecar copy was read from. It only
// labels a rescued recipe so its snapshot name says where it came from.
type Origin int

const (
	// OriginUnknown leaves the rescued snapshot with the plain label.
	OriginUnknown Origin = iota
	// OriginSidecarFolder is <photo-folder>/.hdrtoys/<rel>.edits.
	OriginSidecarFolder
	// OriginBesidePhoto is <photo-folder>/<rel>.edits.
	OriginBesidePhoto
)

// MergeOptions configures Merge behavior.
type MergeOptions struct {
	// RescueConflictingRecipes preserves the losing working recipe as an
	// immutable manual snapshot whenever two truth copies carry distinct
	// recipes, so a merge never destroys a creative state.
	RescueConflictingRecipes bool
	// OriginA and OriginB optionally say where each copy was read from. When
	// both are known, the rescued snapshot's name records the loser's origin.
	OriginA, OriginB Origin
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

	var (
		provenance         map[string]any
		provenanceOther    any
		hasProvenanceOther bool
	)
	if rawProv, exists := obj["provenance"]; exists {
		if pm, ok := rawProv.(map[string]any); ok {
			provenance = cloneMap(pm)
		} else {
			provenanceOther, hasProvenanceOther = rawProv, true
		}
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

		provenanceOther:    provenanceOther,
		hasProvenanceOther: hasProvenanceOther,
	}, nil
}

func parsePositiveInt(v any) (int, error) {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			if i < 1 {
				return 0, errors.New("must be integer >= 1")
			}
			return int(i), nil
		}
		// Integer-valued literals written by other JSON encoders ("1.0", "1e0").
		f, err := n.Float64()
		if err != nil {
			return 0, errors.New("must be integer >= 1")
		}
		return parsePositiveInt(f)
	case float64:
		if n < 1 || n > 1<<31 || n != float64(int(n)) {
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
	v, hasValue := m["v"]
	return FieldCell{
		V:       v,
		HLC:     h,
		Extra:   extra,
		NoValue: !hasValue,
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
	vr := Variant{ID: id, HLC: h, Extra: map[string]any{}}
	liftVariantMembers(&vr, m)
	return vr, nil
}

// liftVariantMembers copies a variant record's members onto vr. "name",
// "recipe", and "tombstone" are lifted into their typed fields only when they
// carry the expected JSON type; any other shape (a null name, a null recipe, a
// false or non-boolean tombstone) stays in Extra so it is re-emitted verbatim
// rather than silently dropped.
func liftVariantMembers(vr *Variant, m map[string]any) {
	for k, val := range m {
		switch k {
		case "id", "hlc":
		case "name":
			if s, ok := val.(string); ok {
				vr.Name = &s
			} else {
				vr.Extra[k] = val
			}
		case "recipe":
			if val != nil {
				vr.Recipe = val
			} else {
				vr.Extra[k] = val
			}
		case "tombstone":
			if b, ok := val.(bool); ok && b {
				vr.Tombstone = true
			} else {
				vr.Extra[k] = val
			}
		default:
			vr.Extra[k] = val
		}
	}
}

// variantTombstoned reports whether a record is deleted. A non-boolean
// "tombstone" member counts when it is truthy by ECMAScript rules, matching
// editors that test the member's truthiness.
func variantTombstoned(v Variant) bool {
	if v.Tombstone {
		return true
	}
	switch t := v.Extra["tombstone"].(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	default:
		return true
	}
}

func fieldCellToMap(c FieldCell) map[string]any {
	m := make(map[string]any, len(c.Extra)+2)
	maps.Copy(m, c.Extra)
	m["hlc"] = c.HLC
	if !c.NoValue {
		m["v"] = c.V
	}
	return m
}

func variantToMap(v Variant, includeTombstone bool) map[string]any {
	m := make(map[string]any, len(v.Extra)+5)
	maps.Copy(m, v.Extra)
	m["id"] = v.ID
	m["hlc"] = v.HLC
	if v.Name != nil {
		m["name"] = *v.Name
	}
	if v.Recipe != nil {
		m["recipe"] = v.Recipe
	}
	switch {
	case !includeTombstone:
		delete(m, "tombstone")
	case v.Tombstone:
		m["tombstone"] = true
	}
	return m
}

// Serialize emits canonical JSON: the bytes JSON.stringify(value, null, 2)
// produces for a recursively key-sorted object with "hdrt" inserted first,
// followed by a trailing "\n". Object keys are ordered as jsjson.SortKeys
// documents (array-index keys ascending, then UTF-16 code-unit order), strings
// and numbers are written as jsjson.AppendString and jsjson.AppendNumberLiteral
// document, and variants are stably sorted by id in UTF-16 code-unit order.
func Serialize(s *Sidecar) ([]byte, error) {
	if s == nil {
		return nil, errors.New("nil sidecar")
	}
	ver := max(s.HDRT, HDRTVersion)

	top := map[string]any{}
	maps.Copy(top, s.Extra)
	delete(top, "hdrt")
	if s.Document != nil {
		docMap := make(map[string]any, len(s.Document.Extra)+1)
		maps.Copy(docMap, s.Document.Extra)
		docMap["role"] = s.Document.Role
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
		sortVariantsByID(sorted)
		arr := make([]any, len(sorted))
		for i, v := range sorted {
			arr[i] = variantToMap(v, true)
		}
		top["variants"] = arr
	}
	switch {
	case s.Provenance != nil:
		top["provenance"] = s.Provenance
	case s.hasProvenanceOther:
		top["provenance"] = s.provenanceOther
	}

	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	jsjson.SortKeys(keys)

	// "hdrt" is inserted first, but an ECMAScript object still enumerates
	// array-index keys ahead of every string key, so those precede it.
	hdrtAt := 0
	for hdrtAt < len(keys) {
		if _, isIndex := jsjson.ArrayIndex(keys[hdrtAt]); !isIndex {
			break
		}
		hdrtAt++
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	writeMemberName := func(name string) {
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		buf.WriteString("\n  ")
		writeJSONString(&buf, name)
		buf.WriteString(": ")
	}
	writeHDRT := func() {
		writeMemberName("hdrt")
		buf.WriteString(strconv.Itoa(ver))
	}
	for i, k := range keys {
		if i == hdrtAt {
			writeHDRT()
		}
		writeMemberName(k)
		if err := writeCanonicalValue(&buf, top[k], 1, true); err != nil {
			return nil, err
		}
	}
	if hdrtAt == len(keys) {
		writeHDRT()
	}
	buf.WriteString("\n}\n")
	return buf.Bytes(), nil
}

// sortVariantsByID stably orders variants by id in UTF-16 code-unit order.
func sortVariantsByID(variants []Variant) {
	sort.SliceStable(variants, func(i, j int) bool {
		return jsjson.CompareUTF16(variants[i].ID, variants[j].ID) < 0
	})
}

// CanonicalCompactJSON returns compact canonical JSON: the bytes
// JSON.stringify produces for a recursively key-sorted value.
func CanonicalCompactJSON(v any) (string, error) {
	var buf bytes.Buffer
	if err := writeCanonicalValue(&buf, v, 0, false); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// canonicalGreater reports whether a sorts strictly after b in the UTF-16
// code-unit order every canonical-JSON tie-break is defined in.
func canonicalGreater(a, b string) bool {
	return jsjson.CompareUTF16(a, b) > 0
}

func writeJSONString(buf *bytes.Buffer, s string) {
	buf.Write(jsjson.AppendString(buf.AvailableBuffer(), s))
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
		writeJSONString(buf, val)
		return nil
	case json.Number:
		out, err := jsjson.AppendNumberLiteral(buf.AvailableBuffer(), val.String())
		if err != nil {
			return fmt.Errorf("invalid number literal %q: %w", val.String(), err)
		}
		buf.Write(out)
		return nil
	case float64:
		buf.Write(jsjson.AppendFloat(buf.AvailableBuffer(), val))
		return nil
	case float32:
		buf.Write(jsjson.AppendFloat(buf.AvailableBuffer(), float64(val)))
		return nil
	case int:
		buf.Write(jsjson.AppendFloat(buf.AvailableBuffer(), float64(val)))
		return nil
	case int64:
		buf.Write(jsjson.AppendFloat(buf.AvailableBuffer(), float64(val)))
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
		jsjson.SortKeys(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if pretty {
				buf.WriteByte('\n')
				writeIndent(buf, depth+1)
			}
			writeJSONString(buf, k)
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
		// Normalize any other typed value through encoding/json with UseNumber
		// so it reaches the canonical writers above.
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
	return canonicalGreater(incBytes, exBytes), nil
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
	vr := Variant{ID: id, HLC: opHLC, Extra: map[string]any{}}
	liftVariantMembers(&vr, m)
	// An op states deletion or nothing: a falsy "tombstone" is not recorded.
	if !variantTombstoned(vr) {
		delete(vr.Extra, "tombstone")
	}
	return vr, nil
}

// JoinVariants computes the join-semilattice of two records with the same ID.
// Payload selects max by (HLC, canonical payload without tombstone);
// Tombstone is logical OR so deletion dominates forever regardless of HLC.
func JoinVariants(existing, incoming Variant) (Variant, error) {
	tomb := variantTombstoned(existing) || variantTombstoned(incoming)
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
		if canonicalGreater(incBytes, exBytes) {
			winner = incoming
		}
	}
	joined := cloneVariant(winner)
	joined.Tombstone = tomb
	// The joined record states its tombstone only as the boolean true.
	delete(joined.Extra, "tombstone")
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

// MergedEditLabel is the name a rescued working recipe carries as a snapshot.
const MergedEditLabel = "Merged edit"

func mergedEditLabel(from Origin) string {
	switch from {
	case OriginSidecarFolder:
		return MergedEditLabel + " \u00b7 from .hdrtoys"
	case OriginBesidePhoto:
		return MergedEditLabel + " \u00b7 from beside the photo"
	default:
		return MergedEditLabel
	}
}

// ConflictRescueVariant constructs the deterministic immutable manual snapshot
// that preserves a losing working recipe. The id is derived from the losing
// cell's content, so merging the same pair of copies again is a byte no-op.
// ok is false when the losing value is not a JSON object: only a recipe object
// is a creative state worth a snapshot.
func ConflictRescueVariant(loser FieldCell, from Origin) (rescued Variant, ok bool) {
	if _, isObject := loser.V.(map[string]any); !isObject {
		return Variant{}, false
	}
	// Identity string: {"hlc":<loser hlc>,"recipe":<loser recipe>} as compact
	// JSON with keys in plain UTF-16 code-unit order.
	identity := []byte(`{"hlc":`)
	identity = jsjson.AppendString(identity, loser.HLC)
	identity = append(identity, `,"recipe":`...)
	identity = appendCodeUnitSortedJSON(identity, loser.V)
	identity = append(identity, '}')

	name := mergedEditLabel(from)
	return Variant{
		ID:     "conflict-" + loser.HLC + "-" + stableHash(string(identity)),
		HLC:    loser.HLC,
		Name:   &name,
		Recipe: loser.V,
		Extra:  map[string]any{"snapshotKind": "manual"},
	}, true
}

// appendCodeUnitSortedJSON appends compact JSON with object keys in plain
// UTF-16 code-unit order (array-index keys are NOT hoisted). It is the input
// of the rescue-id hash only; the file serialization uses writeCanonicalValue.
func appendCodeUnitSortedJSON(dst []byte, v any) []byte {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return jsjson.CompareUTF16(keys[i], keys[j]) < 0 })
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = jsjson.AppendString(dst, k)
			dst = append(dst, ':')
			dst = appendCodeUnitSortedJSON(dst, val[k])
		}
		return append(dst, '}')
	case []any:
		dst = append(dst, '[')
		for i, elem := range val {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendCodeUnitSortedJSON(dst, elem)
		}
		return append(dst, ']')
	default:
		var buf bytes.Buffer
		if err := writeCanonicalValue(&buf, v, 0, false); err != nil {
			return append(dst, "null"...)
		}
		return append(dst, buf.Bytes()...)
	}
}

// stableHash is the 128-bit content hash of the rescue id: four independently
// seeded 32-bit multiplicative lanes over the UTF-16 code units of text,
// rendered as 32 lowercase hex digits.
func stableHash(text string) string {
	lanes := [4]uint32{0x811c9dc5, 0x9e3779b9, 0x85ebca6b, 0xc2b2ae35}
	for _, unit := range utf16.Encode([]rune(text)) {
		for lane := range lanes {
			h := (lanes[lane] ^ (uint32(unit) + uint32(lane)*257)) * 0x01000193
			lanes[lane] = h ^ (h >> 16)
		}
	}
	return fmt.Sprintf("%08x%08x%08x%08x", lanes[0], lanes[1], lanes[2], lanes[3])
}

func sidecarRole(s *Sidecar) string {
	if s.Document != nil && s.Document.Role != "" {
		return s.Document.Role
	}
	return "truth"
}

// provenanceValue returns the sidecar's "provenance" member and whether it is
// present at all (a present null is a value distinct from absence).
func provenanceValue(s *Sidecar) (any, bool) {
	switch {
	case s.Provenance != nil:
		return s.Provenance, true
	case s.hasProvenanceOther:
		return s.provenanceOther, true
	default:
		return nil, false
	}
}

// Merge joins two copies of the same asset's sidecar deterministically. Every
// component joins locally, so the result is independent of exchange order:
//
//	hdrt        max
//	document    role only (truth if either copy is truth)
//	asset       per key: null is bottom, otherwise larger canonical JSON
//	fields      per cell: HLC last-writer-wins, exact ties by canonical cell JSON
//	recipe      the same cell order
//	variants    per id: payload by (hlc, canonical payload JSON), tombstone OR
//	provenance  whole value: null is bottom, otherwise larger canonical JSON
//	other keys  per key, like asset
//
// A portable copy never merges into a truth copy; the truth copy is returned.
func Merge(a, b *Sidecar, opts MergeOptions) (*Sidecar, error) {
	if a == nil {
		return cloneSidecar(b), nil
	}
	if b == nil {
		return cloneSidecar(a), nil
	}
	roleA, roleB := sidecarRole(a), sidecarRole(b)
	if roleA == "portable" && roleB == "truth" {
		return cloneSidecar(b), nil
	}
	if roleA == "truth" && roleB == "portable" {
		return cloneSidecar(a), nil
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

	out := &Sidecar{
		HDRT:     max(a.HDRT, b.HDRT, HDRTVersion),
		Document: &DocumentMeta{Role: role, Extra: map[string]any{}},
		Asset:    asset,
		Fields:   map[string]FieldCell{},
		Extra:    extra,
	}

	provA, hasA := provenanceValue(a)
	provB, hasB := provenanceValue(b)
	if hasA || hasB {
		prov := provA
		switch {
		case !hasA:
			prov = provB
		case hasB:
			if prov, err = joinValues(provA, provB); err != nil {
				return nil, err
			}
		}
		if pm, isObject := prov.(map[string]any); isObject {
			out.Provenance = cloneMap(pm)
		} else {
			out.provenanceOther, out.hasProvenanceOther = prov, true
		}
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
		bWins, err := FieldWins(*b.Recipe, *a.Recipe)
		if err != nil {
			return nil, err
		}
		winner, loser, loserOrigin := *a.Recipe, *b.Recipe, opts.OriginB
		if bWins {
			winner, loser, loserOrigin = *b.Recipe, *a.Recipe, opts.OriginA
		}
		w := cloneFieldCell(winner)
		out.Recipe = &w

		if opts.RescueConflictingRecipes && role == "truth" {
			wBytes, err := CanonicalCompactJSON(winner.V)
			if err != nil {
				return nil, err
			}
			lBytes, err := CanonicalCompactJSON(loser.V)
			if err != nil {
				return nil, err
			}
			if opts.OriginA == OriginUnknown || opts.OriginB == OriginUnknown {
				loserOrigin = OriginUnknown
			}
			if wBytes != lBytes {
				if rv, ok := ConflictRescueVariant(cloneFieldCell(loser), loserOrigin); ok {
					rescued = &rv
				}
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
		sortVariantsByID(joined)
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
	if canonicalGreater(bb, ab) {
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
		V:       c.V,
		HLC:     c.HLC,
		Extra:   cloneMap(c.Extra),
		NoValue: c.NoValue,
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

		provenanceOther:    s.provenanceOther,
		hasProvenanceOther: s.hasProvenanceOther,
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
		if i, err := n.Int64(); err == nil {
			if i < 0 {
				return 0, errors.New("negative")
			}
			return int(i), nil
		}
		// Integer-valued literals from non-browser encoders ("5.0", "5e0").
		f, err := n.Float64()
		if err != nil {
			return 0, errors.New("invalid int")
		}
		return parseNonNegInt(f)
	case float64:
		if n < 0 || n > 1<<31 || n != math.Trunc(n) {
			return 0, errors.New("not a non-negative integer")
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
