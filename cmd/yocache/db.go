package main

// db.go owns the lifecycle of the shared operational SQLite database: opening,
// WAL configuration, and schema migration. All other stores (hashEquivStore,
// blobInventory, …) receive a *sql.DB and never open or close it themselves.

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// openOperationalDB opens (creating if absent) the SQLite database at path.
// WAL mode, a 5 s busy-timeout, and NORMAL sync are wired into the DSN so
// every connection the pool opens inherits them. The caller owns Close.
func openOperationalDB(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db %q: %w", path, err)
	}
	// sql.Open is lazy — Ping forces a real connection so a bad path fails here.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect db %q: %w", path, err)
	}
	return db, nil
}

// migrateDB applies any pending migrations embedded in the binary (migrations/
// directory). Goose tracks applied versions in goose_db_version; re-running is
// safe and a no-op when everything is already current.
func migrateDB(db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
