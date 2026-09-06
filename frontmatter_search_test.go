package main

import (
	"context"
	"os"
	"path/filepath"
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

func fmSearch(t *testing.T, ctx context.Context, in SearchFrontmatterInput) SearchFrontmatterOutput {
	t.Helper()
	_, out, err := SearchFrontmatter(ctx, nil, in)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	return out
}

func fmPaths(out SearchFrontmatterOutput) []string {
	var p []string
	for _, m := range out.Matches {
		p = append(p, m.Path)
	}
	return p
}

func wantPaths(t *testing.T, out SearchFrontmatterOutput, want ...string) {
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
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "subject", Op: "text_eq", Value: "Alpha"}}}), "mail/a.md")
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "subject", Op: "text_contains", Value: "amm"}}}), "mail/c.md")
	// Text comparison is case-sensitive: no smart-casing, no hidden folding.
	if out := fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "subject", Op: "text_eq", Value: "alpha"}}}); out.Total != 0 {
		t.Errorf("text_eq should be case-sensitive, matched %v", fmPaths(out))
	}
}

// exists is about a usable value: a field present but null does not exist.
func TestFMExists(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "attachments.filename", Op: "exists"}}}), "mail/b.md")
	if out := fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "x", Op: "exists"}}}); out.Total != 0 {
		t.Errorf("null field should not exist, matched %v", fmPaths(out))
	}
}

func TestFMNumericOps(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "size", Op: "gt", Value: "150"}}}), "mail/b.md")
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "size", Op: "lte", Value: "100"}}}), "mail/a.md")
	// c.md's size is text: it does not convert, so it does not match — and that
	// is not an error.
	if out := fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "size", Op: "gte", Value: "0"}}}); out.Total != 2 {
		t.Errorf("unconvertible value should just not match: %v", fmPaths(out))
	}
}

func TestFMBoolOp(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "flagged", Op: "bool_eq", Value: "true"}}}), "mail/a.md")
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "flagged", Op: "bool_eq", Value: "false"}}}), "mail/b.md")
}

// The point of separating date_ from time_: a.md is 23:30 on the 2nd and b.md is
// 00:30 on the 3rd, half an hour apart but on different days.
func TestFMDateVsTimeGranularity(t *testing.T) {
	ctx := fmVault(t)
	// Whole days: both a and b are <= the 3rd.
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "date", Op: "date_lte", Value: "2026-01-03"}}}), "mail/a.md", "mail/b.md")
	// The same bound as an instant means midnight, which excludes b.
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "date", Op: "time_lte", Value: "2026-01-03T00:00:00Z"}}}), "mail/a.md")
	// date_eq is day equality, not instant equality.
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "date", Op: "date_eq", Value: "2026-01-02"}}}), "mail/a.md")
}

// A quoted ISO string and a native YAML date are the same type to a date op.
func TestFMDateAcceptsBothYAMLSpellings(t *testing.T) {
	ctx := fmVault(t)
	out := fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "date", Op: "date_gte", Value: "2026-01-03"}}})
	wantPaths(t, out, "mail/b.md", "mail/c.md") // b is a string, c is a time.Time
}

func TestFMFiltersAreANDed(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{Filters: []Filter{
		{Field: "auth.dkim", Op: "text_eq", Value: "pass"},
		{Field: "size", Op: "gt", Value: "50"},
	}}), "mail/a.md")
}

// A dotted path descends lists as well as maps, and matches if any value fits.
func TestFMListDescent(t *testing.T) {
	ctx := fmVault(t)
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "tags", Op: "text_eq", Value: "urgent"}}}), "mail/a.md")
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{
		Filters: []Filter{{Field: "attachments.mime_type", Op: "text_contains", Value: "pdf"}}}), "mail/b.md")
}

// Bad *query* values are caller errors; bad *field* values are not.
func TestFMBadQueryValuesError(t *testing.T) {
	ctx := fmVault(t)
	for _, f := range []Filter{
		{Field: "size", Op: "gt", Value: "big"},
		{Field: "date", Op: "date_lt", Value: "banana"},
		{Field: "flagged", Op: "bool_eq", Value: "yes-ish"},
		{Field: "subject", Op: "nonsense_op", Value: "x"},
		{Field: "", Op: "exists"},
	} {
		if _, _, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{Filters: []Filter{f}}); err == nil {
			t.Errorf("%+v: want an error", f)
		}
	}
}

// Denominators: an empty result must be interpretable.
func TestFMDenominators(t *testing.T) {
	ctx := fmVault(t)
	out := fmSearch(t, ctx, SearchFrontmatterInput{
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
	out := fmSearch(t, ctx, SearchFrontmatterInput{
		Facets: []Facet{{Field: "auth.dkim", Stat: "text_top"}}})
	f := out.Facets[0]
	if f.Docs != 3 || f.Count != 3 || f.Distinct != 2 || f.Unparsed != 0 {
		t.Fatalf("dkim facet: %+v", f)
	}
	if len(f.Top) != 2 || f.Top[0].Value != "pass" || f.Top[0].Count != 2 {
		t.Fatalf("top: %+v", f.Top)
	}
	// A list contributes each element, so count exceeds docs.
	tags := fmSearch(t, ctx, SearchFrontmatterInput{
		Facets: []Facet{{Field: "tags", Stat: "text_top", N: 2}}}).Facets[0]
	if tags.Docs != 2 || tags.Count != 3 || tags.Distinct != 3 || len(tags.Top) != 2 {
		t.Fatalf("tags facet: %+v", tags)
	}
}

func TestFMFacetRangeAndUnparsed(t *testing.T) {
	ctx := fmVault(t)
	f := fmSearch(t, ctx, SearchFrontmatterInput{
		Facets: []Facet{{Field: "size", Stat: "range"}}}).Facets[0]
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
	f := fmSearch(t, ctx, SearchFrontmatterInput{
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
	if m := fmSearch(t, ctx, SearchFrontmatterInput{
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
		{Field: "size", Stat: "range", N: 3},              // n does not apply
	} {
		if _, _, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{Facets: []Facet{f}}); err == nil {
			t.Errorf("%+v: want an error", f)
		}
	}
}

func TestFMSortAndScope(t *testing.T) {
	ctx := fmVault(t)
	// Scope confines the scan.
	out := fmSearch(t, ctx, SearchFrontmatterInput{Path: "mail"})
	wantPaths(t, out, "mail/a.md", "mail/b.md", "mail/c.md")

	// Sort by mtime, newest first.
	now := time.Now()
	for i, p := range []string{"mail/a.md", "mail/b.md", "mail/c.md"} {
		if err := os.Chtimes(filepath.Join(root, p), now, now.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	wantPaths(t, fmSearch(t, ctx, SearchFrontmatterInput{Path: "mail", Sort: "modified", Reverse: true}),
		"mail/c.md", "mail/b.md", "mail/a.md")

	if _, _, err := SearchFrontmatter(ctx, nil, SearchFrontmatterInput{Sort: "date"}); err == nil {
		t.Error("sort by an arbitrary field is not supported; want an error")
	}
}

func TestFMMaxResults(t *testing.T) {
	ctx := fmVault(t)
	zero, two := 0, 2
	// max_results=0 is facets only, and must not read as "unset".
	out := fmSearch(t, ctx, SearchFrontmatterInput{
		Path: "mail", MaxResults: &zero, Facets: []Facet{{Field: "auth.dkim", Stat: "text_top"}}})
	if len(out.Matches) != 0 || out.Total != 3 || !out.Truncated || out.Facets[0].Count != 3 {
		t.Fatalf("facets-only: %+v", out)
	}
	// Facets cover the whole match set, not the returned page.
	out = fmSearch(t, ctx, SearchFrontmatterInput{
		Path: "mail", MaxResults: &two, Facets: []Facet{{Field: "auth.dkim", Stat: "text_top"}}})
	if len(out.Matches) != 2 || out.Total != 3 || out.Facets[0].Count != 3 {
		t.Fatalf("capped page: %+v", out)
	}
}
