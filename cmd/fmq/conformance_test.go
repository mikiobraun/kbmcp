package main

// The conformance suite: SPEC.md as executable cases, run against an fmq
// *binary*, so any implementation can be checked — not just this package.
//
//	FMQ=/path/to/other/fmq go test ./cmd/fmq -run Conformance
//
// Without $FMQ the fmq in this tree is built and run. Each directory under
// conformance/ is one case:
//
//	vault/          the files to scan (absent: an empty directory)
//	query.json      the query document, passed with -query-json
//	dirs.json       optional: the dirs to scan, a JSON list (default: none, i.e. ".")
//	mtimes.json     optional: {"path": "RFC 3339"} overriding the fixed mtime
//	expected.json   the result document fmq must print, compared as JSON
//	error.txt       instead of expected.json: fmq must exit 2. The file says
//	                why, for the reader; its wording is not checked.
//
// Every file in vault/ gets the same fixed mtime before the run, because git
// does not keep mtimes and "modified" is part of the result.
//
// go test ./cmd/fmq -run Conformance -update rewrites expected.json from the
// fmq in this tree. That makes the implementation the spec, so review every
// changed file against SPEC.md before keeping it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite conformance expected.json from this tree's fmq")

// fixedMtime is what every vault file is stamped with unless mtimes.json says
// otherwise.
var fixedMtime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestConformance(t *testing.T) {
	bin := os.Getenv("FMQ")
	if bin == "" {
		bin = filepath.Join(t.TempDir(), "fmq")
		if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
			t.Fatalf("building fmq: %v\n%s", err, out)
		}
	} else if *update {
		t.Fatal("-update regenerates from this tree; unset FMQ")
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}

	cases, err := filepath.Glob("conformance/*/query.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no conformance cases found")
	}
	for _, q := range cases {
		dir := filepath.Dir(q)
		t.Run(filepath.Base(dir), func(t *testing.T) { runCase(t, bin, dir) })
	}
}

func runCase(t *testing.T, bin, dir string) {
	caseDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Run on a copy: stamping mtimes must not touch the checkout.
	work := t.TempDir()
	if src := filepath.Join(caseDir, "vault"); exists(src) {
		if err := os.CopyFS(work, os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
	if err := stampMtimes(work, filepath.Join(caseDir, "mtimes.json")); err != nil {
		t.Fatal(err)
	}

	args := []string{"-query-json", filepath.Join(caseDir, "query.json"), "-o", "json", "--"}
	if p := filepath.Join(caseDir, "dirs.json"); exists(p) {
		var dirs []string
		if err := readJSON(p, &dirs); err != nil {
			t.Fatal(err)
		}
		args = append(args, dirs...)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = work
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	if exists(filepath.Join(caseDir, "error.txt")) {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) || ee.ExitCode() != 2 {
			t.Fatalf("want exit 2, got %v\nstdout: %s", runErr, stdout.String())
		}
		return
	}
	if runErr != nil {
		t.Fatalf("fmq failed: %v\n%s", runErr, stderr.String())
	}

	expectedPath := filepath.Join(caseDir, "expected.json")
	if *update {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, stdout.Bytes(), "", "  "); err != nil {
			t.Fatalf("output is not JSON: %v\n%s", err, stdout.String())
		}
		if err := os.WriteFile(expectedPath, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	var want, got any
	if err := readJSON(expectedPath, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout.String())
	}
	if !reflect.DeepEqual(want, got) {
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("result differs\nwant: %s\ngot:  %s", wantJSON, gotJSON)
	}
}

// stampMtimes gives every file the fixed mtime, then applies overrides.
func stampMtimes(root, overrides string) error {
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(p, fixedMtime, fixedMtime)
	})
	if err != nil || !exists(overrides) {
		return err
	}
	var m map[string]string
	if err := readJSON(overrides, &m); err != nil {
		return err
	}
	for p, s := range m {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return err
		}
		if err := os.Chtimes(filepath.Join(root, p), ts, ts); err != nil {
			return err
		}
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
