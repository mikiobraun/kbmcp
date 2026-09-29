package fmq

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fmVault is a small corpus exercising the shapes real frontmatter takes: a
// quoted timestamp and a native YAML date, nested maps, a list of scalars, a
// list of maps, a bool, a number, a null, and a note with no frontmatter.
func fmVault(t *testing.T) context.Context {
	t.Helper()
	return searchVault(t, map[string]string{
		"mail/a.md":       "---\nsubject: Alpha\ndate: '2026-01-02T23:30:00+00:00'\nsize: 100\nflagged: true\nauth:\n  dkim: pass\ntags: [work, urgent]\nx: null\n---\nbody\n",
		"mail/b.md":       "---\nsubject: Beta\ndate: '2026-01-03T00:30:00+00:00'\nsize: 250\nflagged: false\nauth:\n  dkim: fail\ntags: [home]\nattachments:\n- filename: a.ics\n  mime_type: application/ics\n- filename: b.pdf\n  mime_type: application/pdf\n---\nbody\n",
		"mail/c.md":       "---\nsubject: Gamma\ndate: 2026-01-05\nsize: not-a-number\nauth:\n  dkim: pass\n---\nbody\n",
		"notes/d.md":      "---\ntitle: A note\n---\nbody\n",
		"notes/plain.md":  "no frontmatter here\n",
		"notes/broken.md": "---\nthis: [is not: valid yaml\n---\nbody\n",
	})
}

// searchVault writes files into a temporary directory and makes it the working
// directory, so result paths come out relative to it.
func searchVault(t *testing.T, files map[string]string) context.Context {
	t.Helper()
	dir := t.TempDir()
	for p, content := range files {
		abs := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	return context.Background()
}

func search(ctx context.Context, q Query, dirs ...string) (Result, error) {
	return Search(ctx, q, dirs)
}

func fmSearch(t *testing.T, ctx context.Context, in Query, dirs ...string) Result {
	t.Helper()
	out, err := search(ctx, in, dirs...)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return out
}

func fmPaths(out Result) []string {
	var p []string
	for _, m := range out.Matches {
		p = append(p, m.Path)
	}
	return p
}

func wantPaths(t *testing.T, out Result, want ...string) {
	t.Helper()
	got := fmPaths(out)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestFMTextOps(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "subject", Op: "text_eq", Value: "Alpha"}}}), "mail/a.md")
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "subject", Op: "text_contains", Value: "amm"}}}), "mail/c.md")
	// Text comparison is case-sensitive: no smart-casing, no hidden folding.
	if out := fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "subject", Op: "text_eq", Value: "alpha"}}}); out.Total != 0 {
		t.Errorf("text_eq should be case-sensitive, matched %v", fmPaths(out))
	}
}

// exists is about a usable value: a field present but null does not exist.
func TestFMExists(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "attachments.filename", Op: "exists"}}}), "mail/b.md")
	if out := fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "x", Op: "exists"}}}); out.Total != 0 {
		t.Errorf("null field should not exist, matched %v", fmPaths(out))
	}
}

func TestFMNumericOps(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "size", Op: "int_gt", Value: "150"}}}), "mail/b.md")
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "size", Op: "float_lte", Value: "100.5"}}}), "mail/a.md")
	// c.md's size is text: it does not convert, so it does not match — and that
	// is not an error.
	if out := fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "size", Op: "int_gte", Value: "0"}}}); out.Total != 2 {
		t.Errorf("unconvertible value should just not match: %v", fmPaths(out))
	}
}

func TestFMBoolOp(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "flagged", Op: "bool_eq", Value: "true"}}}), "mail/a.md")
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "flagged", Op: "bool_eq", Value: "false"}}}), "mail/b.md")
}

// The point of separating date_ from time_: a.md is 23:30 on the 2nd and b.md is
// 00:30 on the 3rd, half an hour apart but on different days.
func TestFMDateVsTimeGranularity(t *testing.T) {
	ctx := fmVault(t)
	// Whole days: both a and b are <= the 3rd.
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "date", Op: "date_lte", Value: "2026-01-03"}}}), "mail/a.md", "mail/b.md")
	// The same bound as an instant means midnight, which excludes b.
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "date", Op: "time_lte", Value: "2026-01-03T00:00:00Z"}}}), "mail/a.md")
	// date_eq is day equality, not instant equality.
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "date", Op: "date_eq", Value: "2026-01-02"}}}), "mail/a.md")
}

// A quoted and an unquoted date are the same value to a date op.
func TestFMDateAcceptsBothYAMLSpellings(t *testing.T) {
	ctx := fmVault(t)
	out := fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "date", Op: "date_gte", Value: "2026-01-03"}}})
	wantPaths(t, out, "mail/b.md", "mail/c.md") // b is quoted, c is not
}

// YAML's type resolution is not applied: an unquoted date is still the text it
// was written as, so text_eq finds it by that text.
func TestFMScalarsStayText(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"a.md": "---\ndate: 2026-01-05\nn: 1.0\nmode: 017\n---\n",
		"b.md": "---\ndate: '2026-01-05'\nn: '1.0'\nmode: '017'\n---\n",
	})
	for _, f := range []Filter{
		{Field: "date", Op: "text_eq", Value: "2026-01-05"},
		{Field: "n", Op: "text_eq", Value: "1.0"},
		{Field: "mode", Op: "int_eq", Value: "17"}, // decimal, not YAML 1.1 octal
	} {
		wantPaths(t, fmSearch(t, ctx, Query{Filters: []Filter{f}}), "a.md", "b.md")
	}
}

// Floats are plain decimals: no NaN, Inf, hex or underscores, in the field or
// in the query.
func TestFMFloatGrammar(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"nan.md": "---\nn: .nan\n---\n",
		"NaN.md": "---\nn: NaN\n---\n",
		"inf.md": "---\nn: Inf\n---\n",
		"hex.md": "---\nn: 0x2A\n---\n",
		"und.md": "---\nn: 1_000\n---\n",
		"big.md": "---\nn: 1e999\n---\n",
		"ok.md":  "---\nn: -4.2e1\n---\n",
	})
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "n", Op: "float_lte", Value: "1000"}}}), "ok.md")
	for _, v := range []string{"NaN", "Inf", "0x2A", "1_000", " 5"} {
		if _, err := search(ctx, Query{
			Filters: []Filter{{Field: "n", Op: "float_eq", Value: v}}}); err == nil {
			t.Errorf("query value %q: want an error", v)
		}
	}
}

// Integers compare exactly at any length: these two differ by one but are the
// same float64.
func TestFMIntIsExact(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"a.md":   "---\nid: 9007199254740993\n---\n",
		"b.md":   "---\nid: 9007199254740992\n---\n",
		"c.md":   "---\nid: 123456789012345678901234567890\n---\n",
		"dec.md": "---\nid: 5.0\n---\n",
		"exp.md": "---\nid: 1e3\n---\n",
	})
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "id", Op: "int_eq", Value: "9007199254740993"}}}), "a.md")
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "id", Op: "int_gt", Value: "9007199254740992"}}}), "a.md", "c.md")
	// 5.0 and 1e3 are not integers; float ops read them.
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "id", Op: "int_lte", Value: "1000"}}}))
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "id", Op: "float_lte", Value: "1000"}}}), "dec.md", "exp.md")
	if _, err := search(ctx, Query{
		Filters: []Filter{{Field: "id", Op: "int_eq", Value: "5.0"}}}); err == nil {
		t.Error("int_eq with a decimal query value: want an error")
	}
	f := fmSearch(t, ctx, Query{
		Facets: []Facet{{Field: "id", Stat: "int_range"}}}).Facets[0]
	if f.Min != "9007199254740992" || f.Max != "123456789012345678901234567890" || f.Unparsed != 2 {
		t.Fatalf("int_range: %+v", f)
	}
}

// The unprefixed numeric ops and 'range' are refused with the two typed
// replacements named, so an agent can pick one without guessing.
func TestFMUntypedNumericIsRefused(t *testing.T) {
	ctx := fmVault(t)
	_, err := search(ctx, Query{
		Filters: []Filter{{Field: "size", Op: "lte", Value: "5"}}})
	if err == nil || !strings.Contains(err.Error(), "int_lte") || !strings.Contains(err.Error(), "float_lte") {
		t.Errorf("lte: want an error naming int_lte and float_lte, got %v", err)
	}
	_, err = search(ctx, Query{
		Facets: []Facet{{Field: "size", Stat: "range"}}})
	if err == nil || !strings.Contains(err.Error(), "int_range") || !strings.Contains(err.Error(), "float_range") {
		t.Errorf("range: want an error naming int_range and float_range, got %v", err)
	}
}

func TestFMBoolIsLowercaseOnly(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"a.md": "---\nb: true\n---\n",
		"b.md": "---\nb: 'true'\n---\n",
		"c.md": "---\nb: TRUE\n---\n",
		"d.md": "---\nb: 1\n---\n",
		"e.md": "---\nb: yes\n---\n",
	})
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "b", Op: "bool_eq", Value: "true"}}}), "a.md", "b.md")
	for _, v := range []string{"TRUE", "True", "1", "t"} {
		if _, err := search(ctx, Query{
			Filters: []Filter{{Field: "b", Op: "bool_eq", Value: v}}}); err == nil {
			t.Errorf("query value %q: want an error", v)
		}
	}
}

// Only unquoted empty, ~ and null are absent; anything else is a value.
func TestFMNullSpellings(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"empty.md":  "---\nx:\n---\n",
		"tilde.md":  "---\nx: ~\n---\n",
		"null.md":   "---\nx: null\n---\n",
		"Null.md":   "---\nx: Null\n---\n",
		"quoted.md": "---\nx: 'null'\n---\n",
	})
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "x", Op: "exists"}}}), "Null.md", "quoted.md")
}

// Aliases expand to their anchored value.
func TestFMAliases(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"a.md": "---\nbase: &b {dkim: pass}\ncopy: *b\n---\n",
	})
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "copy.dkim", Op: "text_eq", Value: "pass"}}}), "a.md")
}

// Blocks that have frontmatter but no usable fields count as unparsable, not as
// empty and not as an error.
func TestFMUnusableBlocks(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"unclosed.md": "---\ntitle: never closed\n\nbody: that parses as yaml\n",
		"dup.md":      "---\na: 1\na: 2\n---\n",
		"list.md":     "---\n- a\n- b\n---\n",
		"scalar.md":   "---\njust text\n---\n",
		"loop.md":     "---\na: &x [*x]\n---\n",
		"key.md":      "---\n? [a, b]\n: x\n---\n",
		"empty.md":    "---\n---\n",
	})
	out := fmSearch(t, ctx, Query{})
	wantPaths(t, out, "empty.md")
	if out.WithFrontmatter != 7 || out.Unparsable != 6 {
		t.Errorf("with_frontmatter=%d unparsable=%d; want 7/6", out.WithFrontmatter, out.Unparsable)
	}
}

// A map has no single spelling, so it is not text; a facet counts it unparsed.
func TestFMMapIsNotText(t *testing.T) {
	ctx := fmVault(t)
	f := fmSearch(t, ctx, Query{
		Facets: []Facet{{Field: "auth", Stat: "text_top"}}}).Facets[0]
	if f.Docs != 3 || f.Unparsed != 3 || len(f.Top) != 0 {
		t.Fatalf("auth facet: %+v", f)
	}
}

// No matches is an empty list, not null.
func TestFMNoMatchesIsEmptyList(t *testing.T) {
	ctx := fmVault(t)
	out := fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "subject", Op: "text_eq", Value: "nothing"}}})
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"matches":[]`) {
		t.Errorf("want matches:[], got %s", b)
	}
}

func TestFMFiltersAreANDed(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, Query{Filters: []Filter{
		{Field: "auth.dkim", Op: "text_eq", Value: "pass"},
		{Field: "size", Op: "int_gt", Value: "50"},
	}}), "mail/a.md")
}

// A dotted path descends lists as well as maps, and matches if any value fits.
func TestFMListDescent(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "tags", Op: "text_eq", Value: "urgent"}}}), "mail/a.md")
	wantPaths(t, fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "attachments.mime_type", Op: "text_contains", Value: "pdf"}}}), "mail/b.md")
}

// Bad *query* values are caller errors; bad *field* values are not.
func TestFMBadQueryValuesError(t *testing.T) {
	ctx := fmVault(t)
	for _, f := range []Filter{
		{Field: "size", Op: "int_gt", Value: "big"},
		{Field: "date", Op: "date_lt", Value: "banana"},
		{Field: "flagged", Op: "bool_eq", Value: "yes-ish"},
		{Field: "subject", Op: "nonsense_op", Value: "x"},
		{Field: "", Op: "exists"},
	} {
		if _, err := search(ctx, Query{Filters: []Filter{f}}); err == nil {
			t.Errorf("%+v: want an error", f)
		}
	}
}

// Denominators: an empty result must be interpretable.
func TestFMDenominators(t *testing.T) {
	ctx := fmVault(t)
	out := fmSearch(t, ctx, Query{
		Filters: []Filter{{Field: "subject", Op: "text_eq", Value: "nothing"}}})
	if out.Total != 0 {
		t.Fatalf("want no matches, got %v", fmPaths(out))
	}
	if out.Scanned != 6 || out.WithFrontmatter != 5 || out.Unparsable != 1 {
		t.Errorf("scanned=%d with_frontmatter=%d unparsable=%d; want 6/5/1", out.Scanned, out.WithFrontmatter, out.Unparsable)
	}
}

func TestFMFacetTextTop(t *testing.T) {
	ctx := fmVault(t)
	out := fmSearch(t, ctx, Query{
		Facets: []Facet{{Field: "auth.dkim", Stat: "text_top"}}})
	f := out.Facets[0]
	if f.Docs != 3 || f.Count != 3 || f.Distinct != 2 || f.Unparsed != 0 {
		t.Fatalf("dkim facet: %+v", f)
	}
	if len(f.Top) != 2 || f.Top[0].Value != "pass" || f.Top[0].Count != 2 {
		t.Fatalf("top: %+v", f.Top)
	}
	// A list contributes each element, so count exceeds docs.
	tags := fmSearch(t, ctx, Query{
		Facets: []Facet{{Field: "tags", Stat: "text_top", N: 2}}}).Facets[0]
	if tags.Docs != 2 || tags.Count != 3 || tags.Distinct != 3 || len(tags.Top) != 2 {
		t.Fatalf("tags facet: %+v", tags)
	}
}

func TestFMFacetRangeAndUnparsed(t *testing.T) {
	ctx := fmVault(t)
	f := fmSearch(t, ctx, Query{
		Facets: []Facet{{Field: "size", Stat: "int_range"}}}).Facets[0]
	if f.Min != "100" || f.Max != "250" {
		t.Fatalf("range: %+v", f)
	}
	// c.md's size is text: counted, and reported as unparsed rather than hidden.
	if f.Count != 3 || f.Unparsed != 1 {
		t.Fatalf("want 3 values with 1 unparsed, got %+v", f)
	}
}

func TestFMFacetBins(t *testing.T) {
	ctx := fmVault(t)
	f := fmSearch(t, ctx, Query{
		Facets: []Facet{{Field: "date", Stat: "time_bins", Bin: "day"}}}).Facets[0]
	if f.Min != "2026-01-02T23:30:00Z" || f.Max != "2026-01-05T00:00:00Z" {
		t.Fatalf("bins min/max: %+v", f)
	}
	// Empty bins are omitted, so the 4th is absent and the keys sort in order.
	want := []FacetBin{{"2026-01-02", 1}, {"2026-01-03", 1}, {"2026-01-05", 1}}
	if len(f.Bins) != len(want) {
		t.Fatalf("bins: %+v", f.Bins)
	}
	for i := range want {
		if f.Bins[i] != want[i] {
			t.Fatalf("bins: %+v, want %+v", f.Bins, want)
		}
	}
	if m := fmSearch(t, ctx, Query{
		Facets: []Facet{{Field: "date", Stat: "date_bins", Bin: "month"}}}).Facets[0]; len(m.Bins) != 1 || m.Bins[0].Count != 3 {
		t.Fatalf("month bins: %+v", m.Bins)
	}
}

func TestFMFacetValidation(t *testing.T) {
	ctx := fmVault(t)
	for _, f := range []Facet{
		{Field: "date", Stat: "date_bins"},                // missing bin
		{Field: "date", Stat: "date_bins", Bin: "minute"}, // minute is not a day-level bin
		{Field: "date", Stat: "time_bins", Bin: "week"},   // week carries a convention; not offered
		{Field: "date", Stat: "time_range", Bin: "day"},   // bin does not apply
		{Field: "size", Stat: "top"},                      // unknown stat
		{Field: "size", Stat: "int_range", N: 3},          // n does not apply
	} {
		if _, err := search(ctx, Query{Facets: []Facet{f}}); err == nil {
			t.Errorf("%+v: want an error", f)
		}
	}
}

func TestFMSortAndScope(t *testing.T) {
	ctx := fmVault(t)
	// Scope confines the scan.
	out := fmSearch(t, ctx, Query{}, "mail")
	wantPaths(t, out, "mail/a.md", "mail/b.md", "mail/c.md")

	// Sort by mtime, newest first.
	now := time.Now()
	for i, p := range []string{"mail/a.md", "mail/b.md", "mail/c.md"} {
		if err := os.Chtimes(p, now, now.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	wantPaths(t, fmSearch(t, ctx, Query{Sort: "modified", Reverse: true}, "mail"),
		"mail/c.md", "mail/b.md", "mail/a.md")

	if _, err := search(ctx, Query{Sort: "date"}); err == nil {
		t.Error("sort by an arbitrary field is not supported; want an error")
	}
}

func TestFMMaxResults(t *testing.T) {
	ctx := fmVault(t)
	zero, two := 0, 2
	// max_results=0 is facets only, and must not read as "unset".
	out := fmSearch(t, ctx, Query{
		MaxResults: &zero, Facets: []Facet{{Field: "auth.dkim", Stat: "text_top"}}}, "mail")
	if len(out.Matches) != 0 || out.Total != 3 || !out.Truncated || out.Facets[0].Count != 3 {
		t.Fatalf("facets-only: %+v", out)
	}
	// Facets cover the whole match set, not the returned page.
	out = fmSearch(t, ctx, Query{
		MaxResults: &two, Facets: []Facet{{Field: "auth.dkim", Stat: "text_top"}}}, "mail")
	if len(out.Matches) != 2 || out.Total != 3 || out.Facets[0].Count != 3 {
		t.Fatalf("capped page: %+v", out)
	}
}

// Paths come back as walked, cleaned: relative in, relative out.
func TestFMPathsAsGiven(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, Query{}, "./mail/", "notes/d.md"), "mail/a.md", "mail/b.md", "mail/c.md", "notes/d.md")
}

// A scope that does not exist is an error; an empty result would hide the typo.
func TestFMMissingDirIsError(t *testing.T) {
	ctx := fmVault(t)
	if _, err := search(ctx, Query{}, "mial"); err == nil {
		t.Error("want an error for a missing dir")
	}
}

// Hidden entries the walk discovers are skipped; one named explicitly is not.
func TestFMHidden(t *testing.T) {
	ctx := searchVault(t, map[string]string{
		"a.md":          "---\nx: 1\n---\n",
		".hidden.md":    "---\nx: 1\n---\n",
		".notes/b.md":   "---\nx: 1\n---\n",
		"sub/.git/c.md": "---\nx: 1\n---\n",
	})
	wantPaths(t, fmSearch(t, ctx, Query{}), "a.md")
	wantPaths(t, fmSearch(t, ctx, Query{}, ".notes"), ".notes/b.md")
}

// FilterOps must name exactly the ops compileFilter accepts, or the CLI would
// offer a flag that fails, or lack one for a valid op.
func TestFilterOpsInStep(t *testing.T) {
	valid := map[string]string{"text": "x", "bool": "true", "int": "1", "float": "1", "date": "2026-01-01", "time": "2026-01-01"}
	for _, op := range FilterOps {
		base, _, _ := strings.Cut(op, "_")
		if _, err := compileFilter(Filter{Field: "f", Op: op, Value: valid[base]}); err != nil {
			t.Errorf("%s: %v", op, err)
		}
	}
	// Every family, every comparison: a missing entry shows up as a count.
	if len(FilterOps) != 5+4*5 {
		t.Errorf("FilterOps has %d entries, want %d", len(FilterOps), 5+4*5)
	}
}
