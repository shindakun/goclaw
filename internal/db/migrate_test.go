package db

import (
	"path/filepath"
	"testing"
)

// TestMigrate_AppliesAndTracks verifies migrations run on a fresh DB and are
// recorded in schema_migrations.
func TestMigrate_AppliesAndTracks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "central.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.Close() }()

	// At least the initial migration must be recorded.
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count == 0 {
		t.Fatal("expected at least one applied migration, got 0")
	}

	// A table from 0001_init must exist.
	var name string
	err = d.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='users'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("users table not created by migration: %v", err)
	}
}

// TestMigrate_RunsEachMigrationOnce verifies re-running migrate (as happens on
// every startup) does not re-apply or duplicate anything.
func TestMigrate_RunsEachMigrationOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "central.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.Close() }()

	var before int
	if err := d.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}

	// Calling migrate again must be a no-op (idempotent startup).
	if err := d.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	var after int
	if err := d.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	if before != after {
		t.Fatalf("migrate not idempotent: %d rows before, %d after", before, after)
	}
}

// TestParseVersion covers filename parsing rules.
func TestParseVersion(t *testing.T) {
	cases := []struct {
		name    string
		want    int
		wantErr bool
	}{
		{"0001_init.sql", 1, false},
		{"0042_add_index.sql", 42, false},
		{"noprefix.sql", 0, true},
		{"abcd_bad.sql", 0, true},
	}
	for _, tc := range cases {
		got, err := parseVersion(tc.name)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%q: got version %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestLoadMigrations_OrderedAndParsed ensures the embedded set loads in order.
func TestLoadMigrations_OrderedAndParsed(t *testing.T) {
	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations loaded")
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].version >= all[i].version {
			t.Fatalf("migrations not strictly ordered: %d then %d", all[i-1].version, all[i].version)
		}
	}
}
