package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
