package db

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// migrationsFS holds the ordered SQL migrations. Files are named
// NNNN_description.sql (e.g. 0001_init.sql); the numeric prefix is the version
// and determines apply order. Each file runs exactly once, in a transaction,
// and is recorded in schema_migrations.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration is one parsed migration file.
type migration struct {
	version int
	name    string // full filename, for logging
	sql     string
}

// migrate brings the database up to the latest schema version. It is safe to
// call on every startup: already-applied migrations are skipped, and pending
// ones are applied in version order, each within its own transaction so a
// failure leaves the DB at the last good version.
func (d *DB) migrate() error {
	if err := d.ensureMigrationsTable(); err != nil {
		return err
	}
	applied, err := d.appliedVersions()
	if err != nil {
		return err
	}
	all, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range all {
		if applied[m.version] {
			continue
		}
		if err := d.applyMigration(m); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}
	return nil
}

// ensureMigrationsTable creates the tracking table if it doesn't exist.
func (d *DB) ensureMigrationsTable() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    applied_at  TEXT NOT NULL DEFAULT (datetime('now'))
);`
	if _, err := d.Exec(ddl); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

// appliedVersions returns the set of versions already recorded as applied.
func (d *DB) appliedVersions() (map[int]bool, error) {
	rows, err := d.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// applyMigration runs one migration and records it, atomically. The DDL and the
// schema_migrations insert share a transaction so the version is recorded if
// and only if the migration succeeded.
func (d *DB) applyMigration(m migration) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	if _, err := tx.Exec(m.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
		m.version, m.name,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// loadMigrations reads and parses every embedded migration file, sorted by
// version. It errors on a malformed filename or a duplicate version so problems
// surface at startup rather than silently reordering.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	var out []migration
	seen := make(map[int]string)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, err := parseVersion(name)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %04d: %q and %q", version, prev, name)
		}
		seen[version] = name

		data, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		out = append(out, migration{version: version, name: name, sql: string(data)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// parseVersion extracts the leading integer of a "NNNN_description.sql" name.
func parseVersion(filename string) (int, error) {
	prefix, _, ok := strings.Cut(filename, "_")
	if !ok {
		return 0, fmt.Errorf("migration %q must be named NNNN_description.sql", filename)
	}
	v, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("migration %q has non-numeric version prefix %q", filename, prefix)
	}
	return v, nil
}
