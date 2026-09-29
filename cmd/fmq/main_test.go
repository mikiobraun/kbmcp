package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kbmcp/fmq"
)

// vault writes files into a temporary directory and makes it the working
// directory.
func vault(t *testing.T, files map[string]string) {
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
}

func runFmq(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func jsonResult(t *testing.T, s string) fmq.Result {
	t.Helper()
	var r fmq.Result
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatalf("output is not a result: %v\n%s", err, s)
	}
	return r
}

var twoNotes = map[string]string{
	"notes/a.md": "---\ntitle: Hello world\ntags: [x, y]\n---\n",
	"notes/b.md": "---\ntitle: Other\ntags: [x]\n---\n",
}

// An operand is the argument exactly as the shell passed it: inner and leading
// spaces are part of the value, and nothing is re-split or unquoted.
func TestOperandsAreVerbatim(t *testing.T) {
	vault(t, map[string]string{
		"a.md": "---\ntitle: Hello world\n---\n",
		"b.md": "---\ntitle: ' padded'\n---\n",
		"c.md": "---\ntitle: \"'quoted'\"\n---\n",
	})
	for value, want := range map[string]string{
		"Hello world": "a.md",
		" padded":     "b.md",
		"'quoted'":    "c.md",
	} {
		code, out, errs := runFmq(t, "", "-o", "json", "-text_eq", "title", value)
		if code != 0 {
			t.Fatalf("%q: exit %d: %s", value, code, errs)
		}
		if r := jsonResult(t, out); r.Total != 1 || r.Matches[0].Path != want {
			t.Errorf("%q: got %+v, want %s", value, r, want)
		}
	}
}

// Operands are taken by position, so a value that looks like a flag is a value.
func TestOperandLooksLikeFlag(t *testing.T) {
	vault(t, map[string]string{"a.md": "---\nn: -5\n---\n", "b.md": "---\nn: -o\n---\n"})
	_, out, errs := runFmq(t, "", "-o", "json", "-int_eq", "n", "-5")
	if r := jsonResult(t, out); r.Total != 1 || r.Matches[0].Path != "a.md" {
		t.Fatalf("-int_eq n -5: %+v %s", r, errs)
	}
	_, out, errs = runFmq(t, "", "-o", "json", "-text_eq", "n", "-o")
	if r := jsonResult(t, out); r.Total != 1 || r.Matches[0].Path != "b.md" {
		t.Fatalf("-text_eq n -o: %+v %s", r, errs)
	}
}

// Dirs may come anywhere; after -- even one named like a flag.
func TestDirsAnywhere(t *testing.T) {
	vault(t, map[string]string{
		"notes/a.md": "---\nx: 1\n---\n",
		"-odd/b.md":  "---\nx: 1\n---\n",
		"other/c.md": "---\nx: 1\n---\n",
	})
	_, out, errs := runFmq(t, "", "notes", "-o", "json", "-exists", "x", "--", "-odd")
	r := jsonResult(t, out)
	if r.Total != 2 || r.Matches[0].Path != "-odd/b.md" || r.Matches[1].Path != "notes/a.md" {
		t.Fatalf("got %+v %s", r, errs)
	}
}

func TestFlagsAndQueryJSONAgree(t *testing.T) {
	vault(t, twoNotes)
	_, fromFlags, _ := runFmq(t, "", "-o", "json", "-text_eq", "tags", "y", "-exists", "title", "-f", "tags:text_top:1", "-s", "modified", "-r", "-n", "5", "notes")
	query := `{"filters":[{"field":"tags","op":"text_eq","value":"y"},{"field":"title","op":"exists"}],"facets":[{"field":"tags","stat":"text_top","n":1}],"sort":"modified","sort_reverse":true,"max_results":5}`
	code, fromJSON, errs := runFmq(t, query, "-query-json", "-", "-o", "json", "notes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if fromFlags != fromJSON {
		t.Fatalf("flags and JSON differ:\n%s\n%s", fromFlags, fromJSON)
	}
}

// Errors exit 2 with the message on stderr, where a caller can relay it.
func TestErrorsGoToStderr(t *testing.T) {
	vault(t, twoNotes)
	for _, args := range [][]string{
		{"-lte", "size", "5"},                       // untyped op: the message names both replacements
		{"-text_eq", "title"},                       // missing VALUE
		{"-exists"},                                 // missing FIELD
		{"-text_equal", "title", "x"},               // unknown flag
		{"-f", "size:int_range:3"},                  // int_range takes no argument
		{"-query-json", "-", "-exists", "a"},        // two sources for the query
		{"-o", "yaml"},                              // unknown format
		{"missing-dir"},                             // scope does not exist
		{"-query-json", "-", "-s", "path", "notes"}, // mixing again, other flag
	} {
		code, out, errs := runFmq(t, "{}", args...)
		if code != 2 || out != "" || !strings.HasPrefix(errs, "fmq: ") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, out, errs)
		}
	}
	_, _, errs := runFmq(t, "", "-lte", "size", "5")
	if !strings.Contains(errs, "-int_lte") || !strings.Contains(errs, "-float_lte") {
		t.Errorf("untyped op error should name both replacements: %s", errs)
	}
}

func TestFieldFlag(t *testing.T) {
	vault(t, twoNotes)
	code, out, errs := runFmq(t, "", "-o", "json", "-field", "title", "-field", "tags", "-text_eq", "tags", "y")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	r := jsonResult(t, out)
	if r.Total != 1 || fmt.Sprint(r.Matches[0].Fields) != "map[tags:[x y] title:[Hello world]]" {
		t.Fatalf("got %+v", r.Matches)
	}
	// Text output has no form for field values; the error says what to use.
	if code, _, errs := runFmq(t, "", "-field", "title"); code != 2 || !strings.Contains(errs, "-o json") {
		t.Errorf("text with -field: exit %d, %s", code, errs)
	}
	if code, _, _ := runFmq(t, `{"fields":["title"]}`, "-query-json", "-"); code != 2 {
		t.Errorf("text with fields from -query-json: exit %d", code)
	}
}

// Every op fmq knows is a flag.
func TestEveryOpIsAFlag(t *testing.T) {
	vault(t, twoNotes)
	valid := map[string]string{"text": "x", "bool": "true", "int": "1", "float": "1", "date": "2026-01-01", "time": "2026-01-01"}
	for _, op := range fmq.FilterOps {
		args := []string{"-" + op, "title"}
		if !slices.Contains(fmq.ValuelessOps, op) {
			base, _, _ := strings.Cut(op, "_")
			args = append(args, valid[base])
		}
		if code, _, errs := runFmq(t, "", args...); code != 0 {
			t.Errorf("-%s: exit %d: %s", op, code, errs)
		}
	}
}

// A misspelt key would otherwise be ignored and the query match everything.
func TestQueryJSONRejectsUnknownKeys(t *testing.T) {
	vault(t, twoNotes)
	code, _, errs := runFmq(t, `{"filter":[]}`, "-query-json", "-")
	if code != 2 || !strings.Contains(errs, "filter") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

// stdout is only the paths, so it can feed xargs or a loop; the summary is on
// stderr.
func TestTextOutputIsJustPaths(t *testing.T) {
	vault(t, twoNotes)
	code, out, errs := runFmq(t, "", "-text_eq", "tags", "x")
	if code != 0 || out != "notes/a.md\nnotes/b.md\n" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if !strings.HasPrefix(errs, "2 matches (scanned 2 files") {
		t.Errorf("stderr %q", errs)
	}
}

func TestNulTerminatedPaths(t *testing.T) {
	vault(t, map[string]string{"a b.md": "---\nx: 1\n---\n", "c.md": "---\nx: 1\n---\n"})
	_, out, _ := runFmq(t, "", "-0", "-exists", "x")
	if out != "a b.md\x00c.md\x00" {
		t.Fatalf("stdout %q", out)
	}
}

// A cut-off list is announced where a person sees it, not in the pipe.
func TestTruncationGoesToStderr(t *testing.T) {
	vault(t, twoNotes)
	_, out, errs := runFmq(t, "", "-n", "1")
	if out != "notes/a.md\n" || !strings.Contains(errs, "2 matches") || !strings.Contains(errs, "showing the first 1") {
		t.Fatalf("stdout %q, stderr %q", out, errs)
	}
}

// Facets are the answer when asked for, so they go to stdout.
func TestFacetsOnStdout(t *testing.T) {
	vault(t, twoNotes)
	_, out, _ := runFmq(t, "", "-n", "0", "-f", "tags:text_top")
	if !strings.HasPrefix(out, "tags [text_top]") || !strings.Contains(out, "     2  x\n") {
		t.Fatalf("stdout %q", out)
	}
	_, out, _ = runFmq(t, "", "-f", "tags:text_top")
	if !strings.HasPrefix(out, "notes/a.md\nnotes/b.md\n\ntags [text_top]") {
		t.Fatalf("paths then facets: %q", out)
	}
}

// -0 output must stay parseable by xargs -0, so it refuses company.
func TestNulRefusesFacetsAndJSON(t *testing.T) {
	vault(t, twoNotes)
	for _, args := range [][]string{{"-0", "-f", "tags:text_top"}, {"-0", "-o", "json"}} {
		if code, out, _ := runFmq(t, "", args...); code != 2 || out != "" {
			t.Errorf("%q: exit %d, stdout %q", args, code, out)
		}
	}
}

// The CLI has no default limit: a pipeline wants every match.
func TestNoDefaultLimit(t *testing.T) {
	files := map[string]string{}
	for i := range 1500 {
		files[fmt.Sprintf("n%04d.md", i)] = "---\nx: 1\n---\n"
	}
	vault(t, files)
	_, out, _ := runFmq(t, "", "-exists", "x")
	if n := strings.Count(out, "\n"); n != 1500 {
		t.Fatalf("got %d paths, want 1500", n)
	}
}
