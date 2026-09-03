package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	content := "# a comment\n" +
		"\n" +
		"KB_A=plain\n" +
		"export KB_B=exported\n" +
		"KB_C=\"double quoted\"\n" +
		"KB_D='single'\n" +
		"KB_PRESET=fromfile\n"
	if err := os.WriteFile(envPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Vars the file sets: unset after the test so they don't leak to others.
	for _, k := range []string{"KB_A", "KB_B", "KB_C", "KB_D"} {
		k := k
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	// A var already in the environment must win over the file (t.Setenv restores it).
	t.Setenv("KB_PRESET", "fromenv")

	if err := loadEnvFile(envPath); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ key, want string }{
		{"KB_A", "plain"},
		{"KB_B", "exported"},      // `export ` stripped
		{"KB_C", "double quoted"}, // quotes stripped
		{"KB_D", "single"},
		{"KB_PRESET", "fromenv"}, // real env wins over the file
	} {
		if got := os.Getenv(c.key); got != c.want {
			t.Errorf("%s = %q, want %q", c.key, got, c.want)
		}
	}

	// A missing env file is not an error.
	if err := loadEnvFile(filepath.Join(dir, "nope.env")); err != nil {
		t.Errorf("missing env file should be a no-op, got %v", err)
	}
}
