package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv_ParsesAndRespectsPrecedence(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := `
# a comment
export QUOTED="hello world"
SINGLE='single'
PLAIN=plainvalue
ALREADY_SET=fromfile
`
	if err := os.WriteFile(envFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A real env var must win over the file.
	t.Setenv("ALREADY_SET", "fromenv")
	// Ensure the file-only keys are unset to start.
	for _, k := range []string{"QUOTED", "SINGLE", "PLAIN"} {
		os.Unsetenv(k)
	}

	if err := loadDotEnv(envFile); err != nil {
		t.Fatalf("loadDotEnv: %v", err)
	}

	checks := map[string]string{
		"QUOTED":      "hello world",
		"SINGLE":      "single",
		"PLAIN":       "plainvalue",
		"ALREADY_SET": "fromenv", // env wins
	}
	for k, want := range checks {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestLoadDotEnv_MissingFileIsOK(t *testing.T) {
	if err := loadDotEnv(filepath.Join(t.TempDir(), "nope.env")); err != nil {
		t.Fatalf("missing .env should not error: %v", err)
	}
}
