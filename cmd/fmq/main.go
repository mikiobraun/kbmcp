// fmq queries files by the values in their YAML frontmatter. See SPEC.md.
//
// The JSON query document is the canonical input and the JSON result the
// canonical output; the command line is a thin layer for people and coding
// agents at a shell, and builds the same query document.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"kbmcp/fmq"
)

const usage = `usage: fmq [options] [filters] [dir...]

Scans each dir (default .) for files with YAML frontmatter and prints those
whose frontmatter satisfies every filter. Paths are printed as walked, so
relative dirs give relative paths. stdout holds only the paths (and facets,
if asked for); the summary goes to stderr.

Filters, find-style: the operator is the flag, and its operands are the
following arguments, taken verbatim — quote them for the shell only.

  -exists FIELD               -not_exists FIELD (missing, null or empty list)
  -text_eq FIELD VALUE        -text_contains FIELD VALUE
  -bool_eq FIELD true|false
  -int_eq FIELD N             also -int_lt -int_lte -int_gt -int_gte
  -float_eq FIELD X           also -float_lt -float_lte -float_gt -float_gte
  -date_eq FIELD DATE         also -date_lt -date_lte -date_gt -date_gte
  -time_eq FIELD TIME         also -time_lt -time_lte -time_gt -time_gte

Options:

  -f FIELD:STAT[:ARG]   facet, repeatable. STAT is text_top (ARG: how many),
                        int_range, float_range, date_range, time_range,
                        date_bins or time_bins (ARG: the bin, required)
  -field FIELD          return FIELD's values with each match, repeatable;
                        needs -o json
  -s path|modified      sort order (default path)
  -r                    reverse the sort order
  -n N                  print at most N matches (default: all; 0 = facets only)
  -0                    end each path with NUL instead of newline, for xargs -0
  -query-json FILE      read the query as JSON from FILE ("-" for stdin)
                        instead of filters, -f, -field, -s, -r and -n
  -o text|json          output format (default text)
  --                    everything after is a dir

Example:

  fmq -text_eq authentication.dkim pass -date_gte date 2026-09-01 -s modified -r mails
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// errHelp asks run to print the usage and succeed.
var errHelp = errors.New("help requested")

// run is main without the process: exit code 0 on success, 2 on any error,
// with the message on stderr — which is what a caller like kbmcp relays.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stdin)
	if errors.Is(err, errHelp) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "fmq: %v\n", err)
		return 2
	}
	res, err := fmq.Search(context.Background(), opts.query, opts.dirs)
	if err != nil {
		fmt.Fprintf(stderr, "fmq: %v\n", err)
		return 2
	}
	switch opts.format {
	case "json":
		err = json.NewEncoder(stdout).Encode(res)
	default:
		err = writeText(res, opts.nul, stdout, stderr)
	}
	if err != nil {
		fmt.Fprintf(stderr, "fmq: %v\n", err)
		return 2
	}
	return 0
}

// writeText puts only the answer on stdout — the paths, then any facets — so
// that fmq can feed xargs or a loop. The summary goes to stderr: still seen at a
// terminal, never in a pipe. So does a truncation notice, which matters most
// exactly when the output is being piped somewhere and a cut-off list would
// pass unnoticed.
func writeText(res fmq.Result, nul bool, stdout, stderr io.Writer) error {
	summary := fmq.RenderSummary(res)
	if res.Truncated && len(res.Matches) > 0 {
		summary += fmt.Sprintf(", showing the first %d", len(res.Matches))
	}
	fmt.Fprintln(stderr, summary)

	var b strings.Builder
	end := "\n"
	if nul {
		end = "\x00"
	}
	for _, m := range res.Matches {
		b.WriteString(m.Path)
		b.WriteString(end)
	}
	if len(res.Facets) > 0 {
		if len(res.Matches) > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(fmq.RenderFacets(res))
	}
	_, err := io.WriteString(stdout, b.String())
	return err
}

// untypedOps once meant float. As flags they are refused with both
// replacements named, as the JSON query refuses them.
var untypedOps = []string{"eq", "lt", "lte", "gt", "gte"}

type options struct {
	query  fmq.Query
	dirs   []string
	format string
	nul    bool // -0: NUL-terminated paths
}

// parseArgs reads the command line. Every flag has a fixed number of operands,
// taken as the next arguments whatever they look like (so "-int_gt n -5"
// works). Any other argument is a dir, in any position; a dir that starts with
// "-" goes after "--". Go's flag package is not used: it cannot give a flag
// more than one operand.
func parseArgs(args []string, stdin io.Reader) (options, error) {
	var (
		q          fmq.Query
		dirs       []string
		format     = "text"
		nul        bool
		queryFile  string
		queryFlags []string // flags that build the query, to refuse mixing with -query-json
	)

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			dirs = append(dirs, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			dirs = append(dirs, a)
			continue
		}
		name := strings.TrimPrefix(a, "-")
		name = strings.TrimPrefix(name, "-") // --flag is accepted like -flag

		// operands takes the next n arguments for flag a.
		operands := func(n int, what string) ([]string, error) {
			if i+n >= len(args) {
				return nil, fmt.Errorf("%s needs %s", a, what)
			}
			ops := args[i+1 : i+1+n]
			i += n
			return ops, nil
		}

		switch {
		case name == "h" || name == "help":
			return options{}, errHelp

		case slices.Contains(fmq.ValuelessOps, name):
			ops, err := operands(1, "FIELD")
			if err != nil {
				return options{}, err
			}
			q.Filters = append(q.Filters, fmq.Filter{Field: ops[0], Op: name})
			queryFlags = append(queryFlags, a)

		case slices.Contains(fmq.FilterOps, name):
			ops, err := operands(2, "FIELD VALUE")
			if err != nil {
				return options{}, err
			}
			q.Filters = append(q.Filters, fmq.Filter{Field: ops[0], Op: name, Value: ops[1]})
			queryFlags = append(queryFlags, a)

		case slices.Contains(untypedOps, name):
			return options{}, fmt.Errorf("%s is ambiguous: for integers use -int_%s, for floats use -float_%s", a, name, name)

		case name == "f":
			ops, err := operands(1, "FIELD:STAT[:ARG]")
			if err != nil {
				return options{}, err
			}
			f, err := parseFacet(ops[0])
			if err != nil {
				return options{}, err
			}
			q.Facets = append(q.Facets, f)
			queryFlags = append(queryFlags, a)

		case name == "field":
			ops, err := operands(1, "a FIELD")
			if err != nil {
				return options{}, err
			}
			q.Fields = append(q.Fields, ops[0])
			queryFlags = append(queryFlags, a)

		case name == "s":
			ops, err := operands(1, "path or modified")
			if err != nil {
				return options{}, err
			}
			q.Sort = ops[0]
			queryFlags = append(queryFlags, a)

		case name == "r":
			q.Reverse = true
			queryFlags = append(queryFlags, a)

		case name == "n":
			ops, err := operands(1, "a number")
			if err != nil {
				return options{}, err
			}
			n, err := strconv.Atoi(ops[0])
			if err != nil {
				return options{}, fmt.Errorf("-n: not an integer: %q", ops[0])
			}
			q.MaxResults = &n
			queryFlags = append(queryFlags, a)

		case name == "0":
			nul = true

		case name == "o":
			ops, err := operands(1, "text or json")
			if err != nil {
				return options{}, err
			}
			format = ops[0]

		case name == "query-json":
			ops, err := operands(1, "a FILE, or - for stdin")
			if err != nil {
				return options{}, err
			}
			queryFile = ops[0]

		default:
			return options{}, fmt.Errorf("unknown flag %s (fmq -h lists them; a dir starting with - goes after --)", a)
		}
	}

	if format != "text" && format != "json" {
		return options{}, fmt.Errorf("-o must be text or json, got %q", format)
	}
	if queryFile != "" {
		// One source for the query: mixing would need a rule for which wins.
		if len(queryFlags) > 0 {
			return options{}, fmt.Errorf("-query-json cannot be combined with %s", strings.Join(queryFlags, ", "))
		}
		var err error
		if q, err = readQuery(queryFile, stdin); err != nil {
			return options{}, err
		}
	}
	// Field values are JSON values — lists, nulls, maps — with no text form
	// that would not be invented here and then be hard to take back. jq turns
	// the JSON into whatever a pipeline needs.
	if len(q.Fields) > 0 && format != "json" {
		return options{}, fmt.Errorf("fields are returned only with -o json (e.g. fmq -o json -field title … | jq -r '.matches[] | [.path, .fields.title[0]] | @tsv')")
	}
	// -0 exists so paths survive xargs -0. Facet text or JSON on the same
	// stream would break that, so the combination is refused, not half-done.
	if nul && format == "json" {
		return options{}, fmt.Errorf("-0 applies to text output, not -o json")
	}
	if nul && len(q.Facets) > 0 {
		return options{}, fmt.Errorf("-0 is for piping paths; facets are text and would break it — drop -f or -0")
	}
	return options{query: q, dirs: dirs, format: format, nul: nul}, nil
}

// readQuery decodes the query document. Unknown keys are an error: a misspelt
// "filter" would otherwise silently match everything.
func readQuery(name string, stdin io.Reader) (fmq.Query, error) {
	r := stdin
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return fmq.Query{}, err
		}
		defer f.Close()
		r = f
	}
	var q fmq.Query
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil {
		return fmq.Query{}, fmt.Errorf("query: %v", err)
	}
	if dec.More() {
		return fmq.Query{}, fmt.Errorf("query: more than one JSON document")
	}
	return q, nil
}

// parseFacet reads FIELD:STAT[:ARG]. What ARG means is decided by the stat, so
// the same text is never read two ways; fmq.Search validates the rest.
func parseFacet(s string) (fmq.Facet, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return fmq.Facet{}, fmt.Errorf("-f %q: want FIELD:STAT[:ARG]", s)
	}
	f := fmq.Facet{Field: parts[0], Stat: parts[1]}
	if len(parts) == 3 {
		switch {
		case f.Stat == "text_top":
			n, err := strconv.Atoi(parts[2])
			if err != nil {
				return fmq.Facet{}, fmt.Errorf("-f %q: text_top takes a count, got %q", s, parts[2])
			}
			f.N = n
		case strings.HasSuffix(f.Stat, "_bins"):
			f.Bin = parts[2]
		default:
			return fmq.Facet{}, fmt.Errorf("-f %q: %s takes no argument", s, f.Stat)
		}
	}
	return f, nil
}
