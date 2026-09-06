package main

// search_frontmatter: query notes by the values in their YAML frontmatter, and
// summarise those values with facets.
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

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// defaultFacetTop is how many values a text_top facet returns when the caller
// does not say. Facet output is otherwise unbounded in cardinality.
const defaultFacetTop = 5

type Filter struct {
	Field string `json:"field" jsonschema:"frontmatter field to test; dotted paths descend into maps and lists (e.g. 'authentication.dkim', 'attachments.mime_type')"`
	Op    string `json:"op" jsonschema:"one of: exists; text_eq, text_contains; bool_eq; eq, lt, lte, gt, gte (numeric); date_eq, date_lt, date_lte, date_gt, date_gte (whole days, UTC); time_eq, time_lt, time_lte, time_gt, time_gte (instants). The operator decides how both sides are read; a value that does not convert does not match."`
	Value string `json:"value,omitempty" jsonschema:"value to compare against, always written as a string; the operator decides how to read it. Not used by 'exists'."`
}

type Facet struct {
	Field string `json:"field" jsonschema:"frontmatter field to summarise; dotted paths descend into maps and lists"`
	Stat  string `json:"stat" jsonschema:"one of: text_top (distinct count plus the most common values), range (numeric min/max), date_range, time_range, date_bins, time_bins (histogram plus min/max)"`
	N     int    `json:"n,omitempty" jsonschema:"for text_top: how many values to return (default 5)"`
	Bin   string `json:"bin,omitempty" jsonschema:"for date_bins: day, month or year. For time_bins: minute, hour, day, month or year. Bins are computed in UTC and empty bins are omitted."`
}

type SearchFrontmatterInput struct {
	Filters []Filter `json:"filters,omitempty" jsonschema:"conditions on frontmatter fields, all of which must hold (AND). Empty matches every note that has frontmatter."`
	Facets  []Facet  `json:"facets,omitempty" jsonschema:"summaries to compute over the whole match set (not just the returned page)"`
	Path    string   `json:"path,omitempty" jsonschema:"folder to scope the scan to, relative to root; empty means the whole root"`
	Sort    string   `json:"sort,omitempty" jsonschema:"sort order: 'path' (default) or 'modified' (file's last-change time, which for imported notes is import time, not the note's own date)"`
	Reverse bool     `json:"sort_reverse,omitempty" jsonschema:"reverse the sort order; e.g. sort=modified + sort_reverse=true lists newest first"`
	// A pointer so that "absent" and "zero" are distinguishable: 0 is a useful
	// request (facets only, no document list) and must not read as "unset".
	MaxResults *int `json:"max_results,omitempty" jsonschema:"maximum matches to return (default 200, capped at 1000). Pass 0 for facets only, with no document list."`
}

type FMMatch struct {
	Path     string `json:"path"`
	Modified string `json:"modified"` // RFC3339 UTC, the sort key when sort=modified
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

type SearchFrontmatterOutput struct {
	Matches []FMMatch `json:"matches"`
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

// ---- conversions: each returns false when the value is not of that type ----

func asText(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case time.Time:
		return t.Format(time.RFC3339), true
	case bool:
		return strconv.FormatBool(t), true
	case int:
		return strconv.Itoa(t), true
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), true
	default:
		return fmt.Sprint(t), true
	}
}

func asNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func asBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		return b, err == nil
	default:
		return false, false
	}
}

// timeLayouts are the spellings a date may arrive in. YAML resolves some
// timestamps to time.Time itself, but a quoted one stays a string — the same
// field means the same thing either way, so both are accepted. This is one
// declared target type with several encodings, not type inference.
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
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), true
	case string:
		s := strings.TrimSpace(t)
		for _, layout := range timeLayouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UTC(), true
			}
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

// predicate tests one field value. A filter holds if any value at its path
// satisfies the predicate.
type predicate func(v any) bool

func cmpOrder(op string, c int) bool {
	switch {
	case strings.HasSuffix(op, "_eq"), op == "eq":
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

// compileFilter turns a filter into a predicate, parsing the query value once.
// A query value that cannot be read as the operator's type is a caller error and
// fails the call — unlike a *field* value of the wrong type, which is ordinary
// and simply does not match.
func compileFilter(f Filter) (predicate, error) {
	base, _, _ := strings.Cut(f.Op, "_")
	switch f.Op {
	case "exists":
		return func(v any) bool { return v != nil }, nil

	case "text_eq":
		return func(v any) bool { s, ok := asText(v); return ok && s == f.Value }, nil
	case "text_contains":
		return func(v any) bool { s, ok := asText(v); return ok && strings.Contains(s, f.Value) }, nil

	case "bool_eq":
		want, err := strconv.ParseBool(f.Value)
		if err != nil {
			return nil, fmt.Errorf("bool_eq on %q: value %q is not true or false", f.Field, f.Value)
		}
		return func(v any) bool { b, ok := asBool(v); return ok && b == want }, nil

	case "eq", "lt", "lte", "gt", "gte":
		want, err := strconv.ParseFloat(strings.TrimSpace(f.Value), 64)
		if err != nil {
			return nil, fmt.Errorf("%s on %q: value %q is not a number (numeric ops are unprefixed; use text_eq for text)", f.Op, f.Field, f.Value)
		}
		return func(v any) bool {
			n, ok := asNumber(v)
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
	return nil, fmt.Errorf("unknown op %q on field %q; valid ops: exists, text_eq, text_contains, bool_eq, eq/lt/lte/gt/gte (numeric), date_* and time_* (eq/lt/lte/gt/gte)", f.Op, f.Field)
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
	minN     float64
	maxN     float64
	haveNum  bool
	haveTime bool
}

func validateFacet(f Facet) error {
	switch f.Stat {
	case "text_top", "range", "date_range", "time_range":
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
		return fmt.Errorf("unknown facet stat %q on %q; valid: text_top, range, date_range, time_range, date_bins, time_bins", f.Stat, f.Field)
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
		case "range":
			n, ok := asNumber(v)
			if !ok {
				a.unparsed++
				continue
			}
			if !a.haveNum || n < a.minN {
				a.minN = n
			}
			if !a.haveNum || n > a.maxN {
				a.maxN = n
			}
			a.haveNum = true
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
	case "range":
		if a.haveNum {
			r.Min = strconv.FormatFloat(a.minN, 'g', -1, 64)
			r.Max = strconv.FormatFloat(a.maxN, 'g', -1, 64)
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

// scanDoc reads one file's frontmatter and parses it. ok is false when the file
// has no frontmatter at all; parsed is false when it has one that YAML rejects,
// which the caller counts rather than reporting as a failure — one malformed
// note should not fail a query over the whole vault.
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
	block, has, _ := cutFrontmatter(data, len(buf))
	if !has {
		return nil, false, false
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(block), &m); err != nil {
		return nil, true, false
	}
	// A block that parses to something other than a mapping (a bare list, say)
	// has no fields to query; treat it as present but unusable.
	if m == nil {
		return map[string]any{}, true, true
	}
	return m, true, true
}

func SearchFrontmatter(ctx context.Context, req *mcp.CallToolRequest, in SearchFrontmatterInput) (*mcp.CallToolResult, SearchFrontmatterOutput, error) {
	scope, err := resolve(in.Path)
	if err != nil {
		return nil, SearchFrontmatterOutput{}, err
	}
	switch in.Sort {
	case "", "path", "modified":
	default:
		return nil, SearchFrontmatterOutput{}, fmt.Errorf("sort must be 'path' or 'modified', got %q", in.Sort)
	}

	preds := make([]predicate, len(in.Filters))
	for i, f := range in.Filters {
		if strings.TrimSpace(f.Field) == "" {
			return nil, SearchFrontmatterOutput{}, fmt.Errorf("filter %d: field must not be empty", i)
		}
		p, err := compileFilter(f)
		if err != nil {
			return nil, SearchFrontmatterOutput{}, err
		}
		preds[i] = p
	}
	accs := make([]*facetAcc, len(in.Facets))
	for i, f := range in.Facets {
		if strings.TrimSpace(f.Field) == "" {
			return nil, SearchFrontmatterOutput{}, fmt.Errorf("facet %d: field must not be empty", i)
		}
		if err := validateFacet(f); err != nil {
			return nil, SearchFrontmatterOutput{}, err
		}
		accs[i] = newFacetAcc(f)
	}

	limit := defaultListMax
	if in.MaxResults != nil {
		limit = *in.MaxResults
		if limit < 0 {
			return nil, SearchFrontmatterOutput{}, fmt.Errorf("max_results must not be negative")
		}
		if limit > maxListMax {
			limit = maxListMax
		}
	}

	var out SearchFrontmatterOutput
	// One buffer reused for the whole walk: only a file's head is ever needed,
	// and reading it into a fresh megabyte per note would dominate the scan.
	buf := make([]byte, maxFileBytes)
	walkErr := filepath.WalkDir(scope, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			if p != scope && isHidden(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if isHidden(d.Name()) || !d.Type().IsRegular() {
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
		for i, f := range in.Filters {
			matched := false
			for _, v := range fieldValues(doc, f.Field) {
				if preds[i](v) {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}
		var modified string
		if info, err := d.Info(); err == nil {
			modified = info.ModTime().UTC().Format(time.RFC3339)
		}
		out.Matches = append(out.Matches, FMMatch{Path: relPath(p), Modified: modified})
		for i, f := range in.Facets {
			accs[i].add(fieldValues(doc, f.Field))
		}
		return nil
	})
	if walkErr != nil {
		return nil, SearchFrontmatterOutput{}, walkErr
	}

	sort.Slice(out.Matches, func(i, j int) bool {
		a, b := out.Matches[i], out.Matches[j]
		if in.Sort == "modified" {
			// Path breaks ties so notes sharing a timestamp have a stable order.
			if a.Modified != b.Modified {
				return a.Modified < b.Modified
			}
		}
		return a.Path < b.Path
	})
	if in.Reverse {
		for i, j := 0, len(out.Matches)-1; i < j; i, j = i+1, j-1 {
			out.Matches[i], out.Matches[j] = out.Matches[j], out.Matches[i]
		}
	}

	out.Total = len(out.Matches)
	if len(out.Matches) > limit {
		out.Matches = out.Matches[:limit]
		out.Truncated = true
	}
	for _, a := range accs {
		out.Facets = append(out.Facets, a.result())
	}
	return textResult("%s", renderFrontmatterSearch(out, limit)), out, nil
}

func renderFrontmatterSearch(out SearchFrontmatterOutput, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d matches (scanned %d files, %d with frontmatter", out.Total, out.Scanned, out.WithFrontmatter)
	if out.Unparsable > 0 {
		fmt.Fprintf(&b, ", %d unparsable", out.Unparsable)
	}
	b.WriteString(")\n")
	for _, m := range out.Matches {
		fmt.Fprintf(&b, "  %s\n", m.Path)
	}
	switch {
	case limit == 0 && out.Total > 0:
		// max_results=0 is a deliberate facets-only request, not a truncation
		// the caller should be nudged to undo.
		fmt.Fprintf(&b, "  (facets only; %d matching paths not listed)\n", out.Total)
	case out.Truncated:
		fmt.Fprintf(&b, "  ... (showing %d of %d; raise max_results for more)\n", limit, out.Total)
	}
	for _, f := range out.Facets {
		fmt.Fprintf(&b, "\n%s [%s] — %d values in %d docs, %d distinct", f.Field, f.Stat, f.Count, f.Docs, f.Distinct)
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
