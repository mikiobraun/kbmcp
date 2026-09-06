package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// maxUploadBytes caps a PUT body, mirroring read_file's 1 MiB read cap.
const maxUploadBytes = maxFileBytes

// restGet serves the volume over REST (read-only for now): GET /files/<path>
// returns a file's raw content, or a JSON listing for a directory. It reuses the
// same file layer (path confinement, binary detection) as the MCP tools.
func restGet(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/files/")
	path, err := resolve(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if info.IsDir() {
		restListDir(w, path)
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ct := "text/plain; charset=utf-8"
	switch {
	case looksBinary(data[:min(len(data), 512)]):
		ct = "application/octet-stream"
	case strings.EqualFold(filepath.Ext(path), ".md"):
		ct = "text/markdown; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("ETag", etagFor(info)) // for a future If-Match on PUT
	w.Write(data)
}

// restPut creates or overwrites a file and commits it: PUT /files/<path> with
// the raw new content as the body. Commit metadata rides in the query string
// (?message=… &author_name=… &author_email=…), which keeps the body pure markdown
// and handles arbitrary/multi-line messages that HTTP headers can't. The commit
// mirrors write_file exactly (shared writeAndCommit core).
//
// Conditional requests give agents optimistic locking against each other:
//   - If-Match: <etag>   commit only if the file still matches (else 412)
//   - If-None-Match: *   create only; 412 if the file already exists
//
// The pre-write stat and the write are not atomic, so the check is best-effort —
// enough to catch the common clobber, not a hard lock.
func restPut(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/files/")
	q := r.URL.Query()
	message := q.Get("message")
	if strings.TrimSpace(message) == "" {
		http.Error(w, "missing required query parameter: message", http.StatusBadRequest)
		return
	}

	path, err := resolve(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Conditional preconditions against the current on-disk state.
	info, statErr := os.Stat(path)
	exists := statErr == nil
	if exists && info.IsDir() {
		http.Error(w, fmt.Sprintf("%s is a directory", rel), http.StatusBadRequest)
		return
	}
	if inm := r.Header.Get("If-None-Match"); inm == "*" && exists {
		http.Error(w, "file already exists", http.StatusPreconditionFailed)
		return
	}
	if im := r.Header.Get("If-Match"); im != "" {
		if !exists || (im != "*" && im != etagFor(info)) {
			http.Error(w, "etag precondition failed", http.StatusPreconditionFailed)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxUploadBytes {
		http.Error(w, fmt.Sprintf("body exceeds %d bytes", maxUploadBytes), http.StatusRequestEntityTooLarge)
		return
	}

	res, err := writeAndCommit(rel, string(body), message, q.Get("author_name"), q.Get("author_email"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if newInfo, err := os.Stat(res.Abs); err == nil {
		w.Header().Set("ETag", etagFor(newInfo))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"path":      relPath(res.Abs),
		"created":   res.Created,
		"committed": res.Committed,
	})
}

// restHistory returns recent commits as JSON: GET /history?max=&path=&since=.
// It mirrors the history tool (short hash, dates, author, subject) so the editor
// can show a "recent changes" log without speaking MCP.
func restHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	max := 0
	if s := q.Get("max"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			max = n
		}
	}
	commits, err := commitLog(max, q.Get("since"), q.Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if commits == nil {
		commits = []Commit{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"commits": commits})
}

// restSearch exposes content search: GET /search?substring=…&max=…, returning
// the search tool's own output ({matches:[{path,line,text}], truncated}).
//
// Only substring mode is exposed. The parameter is named for the mode, as the
// tool's field is, so adding ?regex= later says what it means — rather than
// having one "q" whose interpretation depends on a flag somewhere else, which
// is the failure SearchInput's naming was designed to prevent.
func restSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sub := q.Get("substring")
	if strings.TrimSpace(sub) == "" {
		http.Error(w, "missing required query parameter: substring", http.StatusBadRequest)
		return
	}
	max := 0
	if s := q.Get("max"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			max = n
		}
	}

	out, err := searchCore(r.Context(), SearchInput{Substring: sub, MaxResults: max})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// A JSON null would make "no matches" awkward for every caller.
	if out.Matches == nil {
		out.Matches = []Match{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(out)
}

// restFind exposes filename search: GET /find?substring=…&max=…, returning
// {paths:[…], truncated}.
//
// FindInput has no substring mode, only regex and glob — but a filename typed
// into a search box is a literal, and "meeting.md" must not have its '.' read as
// "any character". So the pattern is regex-escaped here: fd matches unanchored,
// which makes an escaped literal exactly a substring match on the name, with the
// escaping done server-side where it is testable. ?regex= and ?glob= stay free
// to be added later meaning precisely what they say.
func restFind(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sub := q.Get("substring")
	if strings.TrimSpace(sub) == "" {
		http.Error(w, "missing required query parameter: substring", http.StatusBadRequest)
		return
	}
	max := 0
	if s := q.Get("max"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			max = n
		}
	}

	out, err := findCore(r.Context(), FindInput{Regex: regexp.QuoteMeta(sub), MaxResults: max})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if out.Paths == nil {
		out.Paths = []string{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(out)
}

// restLinks returns one note's wiki-links and what each resolves to:
// GET /links?path=<note> -> {path, links:[{target,resolved,broken,reason,candidates,line}]}.
//
// It shares outgoingCore with the tool, so a link resolves the same way for a
// browser as for an agent. That path costs one file read and a stat per link —
// no graph, and no other note is read — which is what makes it reasonable to
// call every time a note is opened.
func restLinks(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if strings.TrimSpace(path) == "" {
		http.Error(w, "missing required query parameter: path", http.StatusBadRequest)
		return
	}
	out, err := outgoingCore(r.Context(), path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if out.Links == nil {
		out.Links = []OutLink{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(out)
}

// restListDir returns a JSON listing of a directory's immediate entries.
func restListDir(w http.ResponseWriter, dir string) {
	des, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type entry struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}
	entries := []entry{}
	for _, d := range des {
		if isHidden(d.Name()) {
			continue
		}
		var size int64
		if info, err := d.Info(); err == nil {
			size = info.Size()
		}
		entries = append(entries, entry{
			Name:  d.Name(),
			Path:  relPath(filepath.Join(dir, d.Name())),
			IsDir: d.IsDir(),
			Size:  size,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"entries": entries})
}

// etagFor is a validator derived from mod time + size (for future If-Match writes).
func etagFor(info os.FileInfo) string {
	return fmt.Sprintf(`"%x-%x"`, info.ModTime().UnixNano(), info.Size())
}
