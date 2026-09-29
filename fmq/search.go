// Package fmq queries files by the values in their YAML frontmatter, and
// summarises those values with facets. SPEC.md in cmd/fmq is the contract;
// this is its first implementation.
//
// Two rules shape the whole design:
//
//   - No overloaded operators. An operator names exactly one target type
//     ("date_lte" reads both sides as dates) and converts the field value to it;
//     a value that does not convert simply does not match. There is no type
//     inference, no fallback ladder, and no guessing what the caller meant. The
//     same goes for facets: the caller says which statistic it wants rather than
//     the tool deciding from the data what would look sensible.
//   - Report the numbers, not a verdict. A facet returns counts and denominators
//     so the calling agent can draw its own conclusion — distinct == count with
//     top counts of 1 says "opaque identifier" without this code ever
//     classifying anything.
//
// It is a full scan, deliberately: this serves vaults of hundreds of notes, and
// if that ever stops being true the answer is a real search engine, not a
// hand-rolled index here.
package fmq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultFacetTop is how many values a text_top facet returns when the caller
// does not say. Facet output is otherwise unbounded in cardinality.
const defaultFacetTop = 5

// readLimit is how much of each file is read. Frontmatter lives at the head,
// and a block not closed within it counts as unterminated.
const readLimit = 1 << 20 // 1 MiB

type Filter struct {
	Field string `json:"field" jsonschema:"frontmatter field to test; dotted paths descend into maps and lists (e.g. 'authentication.dkim', 'attachments.mime_type')"`
	Op    string `json:"op" jsonschema:"one of: exists, not_exists (no value, null or empty list); text_eq, text_contains; bool_eq; int_eq, int_lt, int_lte, int_gt, int_gte (exact, any length); float_eq, float_lt, float_lte, float_gt, float_gte (64-bit float); date_eq, date_lt, date_lte, date_gt, date_gte (whole days, UTC); time_eq, time_lt, time_lte, time_gt, time_gte (instants). The operator decides how both sides are read; a value that does not convert does not match."`
	Value string `json:"value,omitempty" jsonschema:"value to compare against, always written as a string; the operator decides how to read it. Not used by 'exists'."`
}

type Facet struct {
	Field string `json:"field" jsonschema:"frontmatter field to summarise; dotted paths descend into maps and lists"`
	Stat  string `json:"stat" jsonschema:"one of: text_top (distinct count plus the most common values), int_range, float_range (min/max), date_range, time_range, date_bins, time_bins (histogram plus min/max)"`
	N     int    `json:"n,omitempty" jsonschema:"for text_top: how many values to return (default 5)"`
	Bin   string `json:"bin,omitempty" jsonschema:"for date_bins: day, month or year. For time_bins: minute, hour, day, month or year. Bins are computed in UTC and empty bins are omitted."`
}

// Query is the canonical input. What to scan is not part of it: the directories
// are given alongside, as they are to rg or fd.
type Query struct {
	Filters []Filter `json:"filters,omitempty"`
	Facets  []Facet  `json:"facets,omitempty"`
	Sort    string   `json:"sort,omitempty"`
	Reverse bool     `json:"sort_reverse,omitempty"`
	// A pointer so that "absent" and "zero" are distinguishable: 0 is a useful
	// request (facets only, no document list) and must not read as "unset".
	MaxResults *int `json:"max_results,omitempty"`
	// Fields to return with each match, so a caller need not read every
	// matched file to see a few values.
	Fields []string `json:"fields,omitempty"`
}

type Match struct {
	Path     string `json:"path"`
	Modified string `json:"modified"` // RFC3339 UTC, the sort key when sort=modified
	// Fields holds, per requested field, the values a filter on it would see:
	// always a list, flattened, [] when missing. Scalars are their text, null is
	// null, a map is itself.
	Fields map[string][]any `json:"fields,omitempty"`
}

type FacetValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type FacetBin struct {
	Bin   string `json:"bin"`
	Count int    `json:"count"`
}

type FacetResult struct {
	Field string `json:"field"`
	Stat  string `json:"stat"`
	// Docs is how many matched documents have the field at all; Count is how
	// many values were seen, which is larger when a field holds a list. Distinct
	// counts distinct textual renderings. Unparsed is the number of values that
	// did not convert to the statistic's type — reported rather than hidden, so
	// a half-empty histogram is visible as such.
	Docs     int `json:"docs"`
	Count    int `json:"count"`
	Distinct int `json:"distinct"`
	Unparsed int `json:"unparsed"`

	Top  []FacetValue `json:"top,omitempty"`
	Min  string       `json:"min,omitempty"`
	Max  string       `json:"max,omitempty"`
	Bins []FacetBin   `json:"bins,omitempty"`
}

// Result is the canonical output.
type Result struct {
	Matches []Match `json:"matches"`
	// Total is the full match count; Matches may be a capped prefix of it.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	// Scanned/WithFrontmatter/Unparsable are the denominators an empty result
	// needs to be interpretable: no matches out of 104 notes with frontmatter
	// means something different from no matches out of none.
	Scanned         int           `json:"scanned"`
	WithFrontmatter int           `json:"with_frontmatter"`
	Unparsable      int           `json:"unparsable"`
	Facets          []FacetResult `json:"facets,omitempty"`
	// FieldCoverage counts, per requested field, the matches — all of them,
	// not just the returned page — that have at least one value for it. It is
	// the denominator for fields, as Total is for matches: 0 of 6 says a field
	// is unused or misspelt, where one page could not.
	// "Has a value" means exactly what exists means.
	FieldCoverage map[string]int `json:"field_coverage,omitempty"`
}

// ---- field access ----

// fieldValues returns every value found at a dotted path. Lists are descended
// implicitly, so "attachments.mime_type" reaches into each element of a list of
// maps; a filter therefore matches if *any* value at the path satisfies it.
// Nils are included: only 'exists' cares about them, and it excludes them.
func fieldValues(doc any, path string) []any {
	cur := []any{doc}
	for _, seg := range strings.Split(path, ".") {
		var next []any
		for _, node := range cur {
			for _, n := range flatten(node) {
				m, ok := n.(map[string]any)
				if !ok {
					continue
				}
				if v, ok := m[seg]; ok {
					next = append(next, v)
				}
			}
		}
		cur = next
	}
	var out []any
	for _, v := range cur {
		out = append(out, flatten(v)...)
	}
	return out
}

// flatten expands a list into its elements (recursively), so a value and a
// one-element list of that value behave alike.
func flatten(v any) []any {
	list, ok := v.([]any)
	if !ok {
		return []any{v}
	}
	var out []any
	for _, e := range list {
		out = append(out, flatten(e)...)
	}
	return out
}

// ---- decoding ----

// decodeFrontmatter parses a block into maps, lists, strings and nils — and
// nothing else. Every scalar stays the text it was written as: YAML's own type
// resolution (unquoted 2026-01-03 is a timestamp, 017 is octal, 1_000 is a
// thousand, TRUE is a bool) is exactly the guessing the typed operators exist
// to avoid, and it would make quoted and unquoted spellings of one value behave
// differently. The operator alone decides how a scalar is read. It also makes
// the semantics reproducible: any YAML parser can report a scalar's text, none
// resolves types the way yaml.v3 does.
func decodeFrontmatter(block string) (map[string]any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 { // empty, or only comments
		return map[string]any{}, nil
	}
	// Aliases expand, so a small block can describe a huge (or, through an
	// alias inside its own anchor, infinite) value. Nothing written out
	// longhand has more nodes than bytes, so that is the budget.
	budget := len(block)
	v, err := fromNode(doc.Content[0], &budget)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("frontmatter is not a mapping")
	}
	return m, nil
}

func fromNode(n *yaml.Node, budget *int) (any, error) {
	if *budget--; *budget < 0 {
		return nil, fmt.Errorf("frontmatter expands beyond its own size (an alias loop?)")
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if isNull(n) {
			return nil, nil
		}
		return n.Value, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fromNode(c, budget)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: a key must be a scalar", k.Line)
			}
			// yaml.v3 accepts a repeated key when decoding to nodes; which
			// value a query would see is not something to leave to chance.
			if _, dup := out[k.Value]; dup {
				return nil, fmt.Errorf("line %d: key %q is defined twice", k.Line, k.Value)
			}
			v, err := fromNode(n.Content[i+1], budget)
			if err != nil {
				return nil, err
			}
			out[k.Value] = v
		}
		return out, nil
	case yaml.AliasNode:
		return fromNode(n.Alias, budget)
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

// isNull is the one piece of YAML type resolution kept: an unquoted empty, ~ or
// null value is absent, which is what 'exists' asks about. Lowercase only, like
// bool: "Null" is text.
func isNull(n *yaml.Node) bool {
	quoted := yaml.SingleQuotedStyle | yaml.DoubleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle
	return n.Style&quoted == 0 && (n.Value == "" || n.Value == "~" || n.Value == "null")
}

// ---- conversions: each returns false when the value is not of that type ----
//
// Values are the strings decodeFrontmatter produced, and query values go
// through the same functions. Each accepts one written grammar exactly — no
// trimming, no alternative spellings.

// asText accepts any scalar. A map is not text: it has no single spelling.
func asText(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// intRE is a plain decimal integer. Integers are compared exactly at any
// length: long identifiers are common in frontmatter, and past 2^53 a float
// would call two different ones equal.
var intRE = regexp.MustCompile(`^-?[0-9]+$`)

func asInt(v any) (*big.Int, bool) {
	s, ok := v.(string)
	if !ok || !intRE.MatchString(s) {
		return nil, false
	}
	return new(big.Int).SetString(s, 10)
}

// floatRE is plain decimal notation. It excludes what strconv.ParseFloat would
// otherwise let through — Inf, NaN, hex floats, underscores — so no value
// reaches a comparison it cannot take part in.
var floatRE = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func asFloat(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok || !floatRE.MatchString(s) {
		return 0, false
	}
	// An exponent out of float64 range is an error here, not an infinity.
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

func asBool(v any) (bool, bool) {
	switch v {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// timeLayouts are the spellings a date may be written in. This is one declared
// target type with several encodings, not type inference.
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02",
}

func asTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	for _, layout := range timeLayouts {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.UTC(), true
		}
	}
	return time.Time{}, false
}

// asDate is asTime truncated to a whole UTC day. Day and instant comparison are
// separate operators precisely so this truncation is the caller's explicit
// choice: comparing a timestamp against a bare date otherwise has to invent an
// answer to "is 2026-01-03 midnight or 23:59?".
func asDate(v any) (time.Time, bool) {
	ts, ok := asTime(v)
	if !ok {
		return time.Time{}, false
	}
	return ts.Truncate(24 * time.Hour), true
}

// ---- filters ----

// predicate tests one field value. For every op but not_exists, a filter holds
// if any value at its path satisfies the predicate.
type predicate func(v any) bool

// filterFn tests all the values at a filter's path.
type filterFn func(vals []any) bool

func anyValue(p predicate) filterFn {
	return func(vals []any) bool {
		for _, v := range vals {
			if p(v) {
				return true
			}
		}
		return false
	}
}

func cmpOrder(op string, c int) bool {
	switch {
	case strings.HasSuffix(op, "_eq"):
		return c == 0
	case strings.HasSuffix(op, "lt"):
		return c < 0
	case strings.HasSuffix(op, "lte"):
		return c <= 0
	case strings.HasSuffix(op, "gt"):
		return c > 0
	case strings.HasSuffix(op, "gte"):
		return c >= 0
	}
	return false
}

// FilterOps lists every filter operator, for callers that build queries in a
// syntax of their own — the CLI makes each one a flag. compileFilter is the
// authority; a test keeps the two in step.
var FilterOps = []string{
	"exists", "not_exists", "text_eq", "text_contains", "bool_eq",
	"int_eq", "int_lt", "int_lte", "int_gt", "int_gte",
	"float_eq", "float_lt", "float_lte", "float_gt", "float_gte",
	"date_eq", "date_lt", "date_lte", "date_gt", "date_gte",
	"time_eq", "time_lt", "time_lte", "time_gt", "time_gte",
}

// ValuelessOps are the ops that take no value: they ask whether a field is
// there, not what it holds.
var ValuelessOps = []string{"exists", "not_exists"}

// compileFilter turns a filter into a test over the values at its path.
//
// not_exists is the one op that is not "any value satisfies": it is the exact
// complement of exists — missing, null and an empty list alike — which no
// per-value predicate can express, since a missing field has no value to test.
func compileFilter(f Filter) (filterFn, error) {
	if f.Op == "not_exists" {
		exists := anyValue(func(v any) bool { return v != nil })
		return func(vals []any) bool { return !exists(vals) }, nil
	}
	p, err := compilePredicate(f)
	if err != nil {
		return nil, err
	}
	return anyValue(p), nil
}

// compilePredicate turns a filter into a predicate, parsing the query value
// once. A query value that cannot be read as the operator's type is a caller
// error and fails the call — unlike a *field* value of the wrong type, which is
// ordinary and simply does not match.
func compilePredicate(f Filter) (predicate, error) {
	base, _, _ := strings.Cut(f.Op, "_")
	switch f.Op {
	case "exists":
		return func(v any) bool { return v != nil }, nil

	case "text_eq":
		return func(v any) bool { s, ok := asText(v); return ok && s == f.Value }, nil
	case "text_contains":
		return func(v any) bool { s, ok := asText(v); return ok && strings.Contains(s, f.Value) }, nil

	case "bool_eq":
		want, ok := asBool(f.Value)
		if !ok {
			return nil, fmt.Errorf("bool_eq on %q: value %q is not true or false", f.Field, f.Value)
		}
		return func(v any) bool { b, ok := asBool(v); return ok && b == want }, nil

	// The unprefixed forms once meant float. They are refused rather than
	// quietly given a meaning, and the error names both replacements so an
	// agent holding an old description can adapt in one step.
	case "eq", "lt", "lte", "gt", "gte":
		return nil, fmt.Errorf("op %q on %q is ambiguous: for integers use int_%s, for floats use float_%s", f.Op, f.Field, f.Op, f.Op)

	case "int_eq", "int_lt", "int_lte", "int_gt", "int_gte":
		want, ok := asInt(f.Value)
		if !ok {
			return nil, fmt.Errorf("%s on %q: value %q is not an integer (use float_%s for decimals)", f.Op, f.Field, f.Value, strings.TrimPrefix(f.Op, "int_"))
		}
		return func(v any) bool {
			n, ok := asInt(v)
			return ok && cmpOrder(f.Op, n.Cmp(want))
		}, nil

	case "float_eq", "float_lt", "float_lte", "float_gt", "float_gte":
		want, ok := asFloat(f.Value)
		if !ok {
			return nil, fmt.Errorf("%s on %q: value %q is not a decimal number (use text_eq for text)", f.Op, f.Field, f.Value)
		}
		return func(v any) bool {
			n, ok := asFloat(v)
			return ok && cmpOrder(f.Op, cmpFloat(n, want))
		}, nil

	case "date_eq", "date_lt", "date_lte", "date_gt", "date_gte",
		"time_eq", "time_lt", "time_lte", "time_gt", "time_gte":
		conv := asTime
		if base == "date" {
			conv = asDate
		}
		want, ok := conv(f.Value)
		if !ok {
			return nil, fmt.Errorf("%s on %q: value %q is not a date/time (try 2026-01-03 or 2026-01-03T15:04:05Z)", f.Op, f.Field, f.Value)
		}
		return func(v any) bool {
			ts, ok := conv(v)
			return ok && cmpOrder(f.Op, ts.Compare(want))
		}, nil
	}
	return nil, fmt.Errorf("unknown op %q on field %q; valid ops: exists, not_exists, text_eq, text_contains, bool_eq, and int_*, float_*, date_*, time_* (each eq/lt/lte/gt/gte)", f.Op, f.Field)
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ---- facets ----

// binLayouts render a timestamp as its bucket key. The keys sort lexically in
// chronological order, so a sorted histogram needs no date-aware comparison.
var binLayouts = map[string]string{
	"minute": "2006-01-02T15:04",
	"hour":   "2006-01-02T15",
	"day":    "2006-01-02",
	"month":  "2006-01",
	"year":   "2006",
}

// binsFor says which bucket sizes a stat accepts. Weeks are deliberately absent:
// unlike the others they carry a convention (ISO Monday vs Sunday) that would
// have to be guessed or configured.
var binsFor = map[string][]string{
	"date_bins": {"day", "month", "year"},
	"time_bins": {"minute", "hour", "day", "month", "year"},
}

type facetAcc struct {
	spec     Facet
	docs     int
	count    int
	unparsed int
	seen     map[string]int // distinct textual renderings, and the text_top tally
	bins     map[string]int
	min, max time.Time
	minI     *big.Int // int_range; nil until a value converts
	maxI     *big.Int
	minF     float64
	maxF     float64
	haveF    bool
	haveTime bool
}

func validateFacet(f Facet) error {
	switch f.Stat {
	case "range":
		return fmt.Errorf("facet stat \"range\" on %q is ambiguous: for integers use int_range, for floats use float_range", f.Field)
	case "text_top", "int_range", "float_range", "date_range", "time_range":
		if f.Bin != "" {
			return fmt.Errorf("facet on %q: 'bin' applies only to date_bins and time_bins", f.Field)
		}
	case "date_bins", "time_bins":
		allowed := binsFor[f.Stat]
		if f.Bin == "" {
			return fmt.Errorf("facet %s on %q needs a 'bin': one of %s", f.Stat, f.Field, strings.Join(allowed, ", "))
		}
		if !contains(allowed, f.Bin) {
			return fmt.Errorf("facet %s on %q: bin %q is not one of %s", f.Stat, f.Field, f.Bin, strings.Join(allowed, ", "))
		}
	default:
		return fmt.Errorf("unknown facet stat %q on %q; valid: text_top, int_range, float_range, date_range, time_range, date_bins, time_bins", f.Stat, f.Field)
	}
	if f.N != 0 && f.Stat != "text_top" {
		return fmt.Errorf("facet on %q: 'n' applies only to text_top", f.Field)
	}
	if f.N < 0 {
		return fmt.Errorf("facet on %q: n must not be negative", f.Field)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func newFacetAcc(f Facet) *facetAcc {
	return &facetAcc{spec: f, seen: map[string]int{}, bins: map[string]int{}}
}

// add folds one document's values for this facet's field into the accumulator.
func (a *facetAcc) add(vals []any) {
	if len(vals) == 0 {
		return
	}
	a.docs++
	for _, v := range vals {
		a.count++
		if s, ok := asText(v); ok {
			a.seen[s]++
		}
		switch a.spec.Stat {
		case "text_top":
			if _, ok := asText(v); !ok {
				a.unparsed++
			}
		case "int_range":
			n, ok := asInt(v)
			if !ok {
				a.unparsed++
				continue
			}
			if a.minI == nil || n.Cmp(a.minI) < 0 {
				a.minI = n
			}
			if a.maxI == nil || n.Cmp(a.maxI) > 0 {
				a.maxI = n
			}
		case "float_range":
			n, ok := asFloat(v)
			if !ok {
				a.unparsed++
				continue
			}
			if !a.haveF || n < a.minF {
				a.minF = n
			}
			if !a.haveF || n > a.maxF {
				a.maxF = n
			}
			a.haveF = true
		default: // date_range, time_range, date_bins, time_bins
			conv := asTime
			if strings.HasPrefix(a.spec.Stat, "date") {
				conv = asDate
			}
			ts, ok := conv(v)
			if !ok {
				a.unparsed++
				continue
			}
			if !a.haveTime || ts.Before(a.min) {
				a.min = ts
			}
			if !a.haveTime || ts.After(a.max) {
				a.max = ts
			}
			a.haveTime = true
			if layout, isBin := binLayouts[a.spec.Bin]; isBin && strings.HasSuffix(a.spec.Stat, "_bins") {
				a.bins[ts.Format(layout)]++
			}
		}
	}
}

func (a *facetAcc) result() FacetResult {
	r := FacetResult{
		Field: a.spec.Field, Stat: a.spec.Stat,
		Docs: a.docs, Count: a.count, Distinct: len(a.seen), Unparsed: a.unparsed,
	}
	switch a.spec.Stat {
	case "text_top":
		n := a.spec.N
		if n == 0 {
			n = defaultFacetTop
		}
		type kv struct {
			k string
			c int
		}
		all := make([]kv, 0, len(a.seen))
		for k, c := range a.seen {
			all = append(all, kv{k, c})
		}
		// Count descending, then value ascending so equal counts have a stable
		// order rather than Go's randomised map iteration.
		sort.Slice(all, func(i, j int) bool {
			if all[i].c != all[j].c {
				return all[i].c > all[j].c
			}
			return all[i].k < all[j].k
		})
		if len(all) > n {
			all = all[:n]
		}
		for _, e := range all {
			r.Top = append(r.Top, FacetValue{Value: e.k, Count: e.c})
		}
	case "int_range":
		if a.minI != nil {
			r.Min = a.minI.String()
			r.Max = a.maxI.String()
		}
	case "float_range":
		if a.haveF {
			r.Min = strconv.FormatFloat(a.minF, 'g', -1, 64)
			r.Max = strconv.FormatFloat(a.maxF, 'g', -1, 64)
		}
	default:
		if a.haveTime {
			layout := time.RFC3339
			if strings.HasPrefix(a.spec.Stat, "date") {
				layout = "2006-01-02"
			}
			r.Min = a.min.Format(layout)
			r.Max = a.max.Format(layout)
		}
		if strings.HasSuffix(a.spec.Stat, "_bins") {
			keys := make([]string, 0, len(a.bins))
			for k := range a.bins {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				r.Bins = append(r.Bins, FacetBin{Bin: k, Count: a.bins[k]})
			}
		}
	}
	return r
}

// ---- the scan ----

// scanDoc reads one file's frontmatter and parses it. has is false when the file
// has no frontmatter at all; parsed is false when it has one that cannot be
// used — rejected by YAML, not a mapping, or never closed — which the caller
// counts rather than reporting as a failure: one malformed note should not fail
// a query over the whole vault.
func scanDoc(abs string, buf []byte) (doc map[string]any, has, parsed bool) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, false, false
	}
	defer f.Close()
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, false, false
	}
	data := buf[:n]
	if looksBinary(data[:min(len(data), 512)]) {
		return nil, false, false
	}
	block, has, closed := cutFrontmatter(data)
	if !has {
		return nil, false, false
	}
	// An unclosed block runs to the end of what was read, so parsing it would
	// read the note's body as YAML — and a body that happens to parse would
	// be queried as frontmatter.
	if !closed {
		return nil, true, false
	}
	m, err := decodeFrontmatter(string(block))
	if err != nil {
		return nil, true, false
	}
	return m, true, true
}

// fmDelim is the frontmatter delimiter line. Only "---" is recognised; YAML's
// "..." document end is legal but unseen in practice.
var fmDelim = []byte("---")

// cutFrontmatter returns the block between a "---" line at byte 0 and the next
// "---" line, delimiters excluded. has reports the opening delimiter, closed
// whether the block ends within data.
func cutFrontmatter(data []byte) (block []byte, has, closed bool) {
	line, rest, _ := bytes.Cut(data, []byte("\n"))
	if !bytes.Equal(bytes.TrimSuffix(line, []byte("\r")), fmDelim) {
		return nil, false, false
	}
	for off := 0; off < len(rest); {
		line, _, found := bytes.Cut(rest[off:], []byte("\n"))
		if bytes.Equal(bytes.TrimSuffix(line, []byte("\r")), fmDelim) {
			return rest[:off], true, true
		}
		if !found {
			break
		}
		off += len(line) + 1
	}
	return nil, true, false
}

// looksBinary reports whether b appears to be binary (contains a NUL byte).
func looksBinary(b []byte) bool {
	return bytes.IndexByte(b, 0) != -1
}

// isHidden reports whether a file or directory name is a dotfile. A hidden
// directory is pruned entirely, which also keeps a .git tree out of a scan.
func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

// Search scans dirs (files are allowed too) and answers q. Output paths are
// the walked paths, cleaned and '/'-separated: relative in, relative out, as
// with rg and fd. A dir that does not exist is an error, not an empty result —
// a mistyped scope would otherwise look like a vault with no matches.
func Search(ctx context.Context, q Query, dirs []string) (Result, error) {
	switch q.Sort {
	case "", "path", "modified":
	default:
		return Result{}, fmt.Errorf("sort must be 'path' or 'modified', got %q", q.Sort)
	}

	filters := make([]filterFn, len(q.Filters))
	for i, f := range q.Filters {
		if strings.TrimSpace(f.Field) == "" {
			return Result{}, fmt.Errorf("filter %d: field must not be empty", i)
		}
		fn, err := compileFilter(f)
		if err != nil {
			return Result{}, err
		}
		filters[i] = fn
	}
	for i, name := range q.Fields {
		if strings.TrimSpace(name) == "" {
			return Result{}, fmt.Errorf("fields %d: field must not be empty", i)
		}
	}
	accs := make([]*facetAcc, len(q.Facets))
	for i, f := range q.Facets {
		if strings.TrimSpace(f.Field) == "" {
			return Result{}, fmt.Errorf("facet %d: field must not be empty", i)
		}
		if err := validateFacet(f); err != nil {
			return Result{}, err
		}
		accs[i] = newFacetAcc(f)
	}

	// No limit unless the query sets one: a pipeline wants every match. A
	// caller with a budget — an agent's context window — sets its own.
	limit := -1
	if q.MaxResults != nil {
		limit = *q.MaxResults
		if limit < 0 {
			return Result{}, fmt.Errorf("max_results must not be negative")
		}
	}

	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			return Result{}, err
		}
	}

	// Matches starts non-nil so that no matches is [] in JSON, not null.
	out := Result{Matches: []Match{}}
	if len(q.Fields) > 0 {
		// Every requested field gets a count, 0 included: a 0 is the point.
		out.FieldCoverage = make(map[string]int, len(q.Fields))
		for _, name := range q.Fields {
			out.FieldCoverage[name] = 0
		}
	}
	// One buffer reused for the whole walk: only a file's head is ever needed,
	// and reading it into a fresh megabyte per note would dominate the scan.
	buf := make([]byte, readLimit)
	for _, dir := range dirs {
		walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable entries are skipped, not fatal
			}
			// What the caller named explicitly is scanned even if hidden; only
			// what the walk discovers is filtered.
			if p != dir && isHidden(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			out.Scanned++
			doc, has, parsed := scanDoc(p, buf)
			if !has {
				return nil
			}
			out.WithFrontmatter++
			if !parsed {
				out.Unparsable++
				return nil
			}
			for i, f := range q.Filters {
				if !filters[i](fieldValues(doc, f.Field)) {
					return nil
				}
			}
			var modified string
			if info, err := d.Info(); err == nil {
				modified = info.ModTime().UTC().Format(time.RFC3339)
			}
			m := Match{Path: filepath.ToSlash(filepath.Clean(p)), Modified: modified}
			if len(q.Fields) > 0 {
				m.Fields = make(map[string][]any, len(q.Fields))
				for _, name := range q.Fields {
					// Non-nil, so a missing field is [] in JSON rather than null.
					m.Fields[name] = append([]any{}, fieldValues(doc, name)...)
				}
				// Counted from the map, so a field requested twice counts once.
				// "Has a value" is exactly exists: a null is not one.
				for name, vals := range m.Fields {
					if slices.ContainsFunc(vals, func(v any) bool { return v != nil }) {
						out.FieldCoverage[name]++
					}
				}
			}
			out.Matches = append(out.Matches, m)
			for i, f := range q.Facets {
				accs[i].add(fieldValues(doc, f.Field))
			}
			return nil
		})
		if walkErr != nil {
			return Result{}, walkErr
		}
	}

	sort.Slice(out.Matches, func(i, j int) bool {
		a, b := out.Matches[i], out.Matches[j]
		if q.Sort == "modified" {
			// Path breaks ties so notes sharing a timestamp have a stable order.
			if a.Modified != b.Modified {
				return a.Modified < b.Modified
			}
		}
		return a.Path < b.Path
	})
	if q.Reverse {
		for i, j := 0, len(out.Matches)-1; i < j; i, j = i+1, j-1 {
			out.Matches[i], out.Matches[j] = out.Matches[j], out.Matches[i]
		}
	}

	out.Total = len(out.Matches)
	if limit >= 0 && len(out.Matches) > limit {
		out.Matches = out.Matches[:limit]
		out.Truncated = true
	}
	for _, a := range accs {
		out.Facets = append(out.Facets, a.result())
	}
	return out, nil
}

// Render is the human- and agent-readable form of a result.
// It is everything in one text, for an agent; a shell wants the parts apart,
// which RenderSummary and RenderFacets give.
func Render(out Result) string {
	var b strings.Builder
	b.WriteString(RenderSummary(out))
	b.WriteByte('\n')
	// A field with no values is left off its line, so a wide projection over
	// sparse fields stays readable. What that leaves unsaid is stated as counts
	// instead, over the whole match set like every other number here. The same
	// counts show a sparse field and a misspelt one (0/12) — reported, never
	// guessed at.
	if len(out.FieldCoverage) > 0 && out.Total > 0 {
		names := make([]string, 0, len(out.FieldCoverage))
		for name := range out.FieldCoverage {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, len(names))
		for i, name := range names {
			parts[i] = fmt.Sprintf("%s %d/%d", name, out.FieldCoverage[name], out.Total)
		}
		fmt.Fprintf(&b, "  matches with a value: %s\n", strings.Join(parts, " · "))
	}
	for _, m := range out.Matches {
		fmt.Fprintf(&b, "  %s", m.Path)
		// Fields sorted by name. A value list is shown as JSON, which says
		// unambiguously what is one value and what is two.
		names := make([]string, 0, len(m.Fields))
		for name, vals := range m.Fields {
			if len(vals) > 0 {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			vals, _ := json.Marshal(m.Fields[name])
			fmt.Fprintf(&b, "  %s: %s", name, vals)
		}
		b.WriteByte('\n')
	}
	switch {
	case len(out.Matches) == 0 && out.Total > 0:
		// Only max_results=0 lists nothing while matching something. It is a
		// deliberate facets-only request, not a truncation the caller should
		// be nudged to undo.
		fmt.Fprintf(&b, "  (facets only; %d matching paths not listed)\n", out.Total)
	case out.Truncated:
		fmt.Fprintf(&b, "  ... (showing %d of %d; raise max_results for more)\n", len(out.Matches), out.Total)
	}
	if len(out.Facets) > 0 {
		b.WriteByte('\n')
		b.WriteString(RenderFacets(out))
	}
	return b.String()
}

// RenderSummary is the one-line account of the scan, without a newline: the
// denominators that make an empty result interpretable.
func RenderSummary(out Result) string {
	s := fmt.Sprintf("%d matches (scanned %d files, %d with frontmatter", out.Total, out.Scanned, out.WithFrontmatter)
	if out.Unparsable > 0 {
		s += fmt.Sprintf(", %d unparsable", out.Unparsable)
	}
	return s + ")"
}

// RenderFacets renders each facet as a block, blocks separated by a blank line.
func RenderFacets(out Result) string {
	var b strings.Builder
	for i, f := range out.Facets {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s [%s] — %d values in %d docs, %d distinct", f.Field, f.Stat, f.Count, f.Docs, f.Distinct)
		if f.Unparsed > 0 {
			fmt.Fprintf(&b, ", %d not %s", f.Unparsed, strings.TrimSuffix(strings.TrimSuffix(f.Stat, "_bins"), "_range"))
		}
		b.WriteByte('\n')
		if f.Min != "" || f.Max != "" {
			fmt.Fprintf(&b, "  range: %s .. %s\n", f.Min, f.Max)
		}
		for _, v := range f.Top {
			fmt.Fprintf(&b, "  %6d  %s\n", v.Count, v.Value)
		}
		for _, v := range f.Bins {
			fmt.Fprintf(&b, "  %6d  %s\n", v.Count, v.Bin)
		}
	}
	return b.String()
}
