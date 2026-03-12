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

	queries, _ := SetupTestDBWithConn(t)
	return queries
}

func SetupTestDBWithConn(t *testing.T) (*sqlc.Queries, *sql.DB) {
	t.Helper()

	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	if _, err := conn.ExecContext(t.Context(), "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}

	if err := migrate(t.Context(), conn, loadMigrations()); err != nil {
		t.Fatal(err)
	}

	return sqlc.New(conn), conn
}
