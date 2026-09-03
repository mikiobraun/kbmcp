package main

import (
	"bufio"
	"os"
	"strings"
)

// loadEnvFile loads KEY=VALUE lines from path into the process environment
// without overriding variables already set — the real environment wins, so an
// env file is a fallback (values may instead come from the shell, systemd, or a
// compose `environment:`). A missing file is not an error; the env file is
// optional.
//
// Format: blank lines and # comments are skipped; an optional leading `export `
// is stripped; values may be single- or double-quoted (quotes are removed). It's
// intentionally minimal — enough for KEY=token style config, not a full dotenv.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		val = unquote(strings.TrimSpace(val))
		// Real env wins: only fill in what isn't already set.
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}

// unquote strips a matching pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
