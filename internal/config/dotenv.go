package config

import (
	"bufio"
	"errors"
	"os"
	"strings"
)

// loadDotEnv reads KEY=VALUE lines from path into the process environment for
// any key not already set, so a real environment variable always wins over the
// file. A missing file is not an error (the file is optional). This is a tiny
// loader on purpose - no dependency, keeping the "few dependencies" ethos
// (brief §2). It supports `#` comments, blank lines, optional `export `
// prefixes, and single/double-quoted values.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
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
			continue // ignore malformed lines rather than failing startup
		}
		key = strings.TrimSpace(key)
		val = unquote(strings.TrimSpace(val))
		if key == "" {
			continue
		}
		if _, present := os.LookupEnv(key); present {
			continue // real env var wins
		}
		os.Setenv(key, val)
	}
	return sc.Err()
}

// unquote strips a single matching pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
