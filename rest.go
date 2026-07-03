package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

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
