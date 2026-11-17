package database

import (
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/110y/muxac/internal/database/sqlc"
)

func loadMigrations() fs.FS {
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(filename), "..", "..", "db", "migrations")
	return os.DirFS(dir)
}

func SetupTestDB(t *testing.T) *sqlc.Queries {
	t.Helper()

	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// Every connection to ":memory:" opens a database of its own: tests that
	// query from several goroutines must share one.
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })

	if _, err := conn.ExecContext(t.Context(), "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}

	if err := migrate(t.Context(), conn, loadMigrations()); err != nil {
		t.Fatal(err)
	}

	return sqlc.New(conn)
}
