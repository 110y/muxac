package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/110y/muxac/internal/database/sqlc"
)

func LoadDDL() (string, error) {
	_, filename, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "db", "schema.sql"))
	if err != nil {
		return "", fmt.Errorf("read schema.sql: %w", err)
	}
	return string(data), nil
}

func SetupTestDB(t *testing.T) *sqlc.Queries {
	t.Helper()

	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	if _, err := conn.ExecContext(t.Context(), "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}

	ddl, err := LoadDDL()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := conn.ExecContext(t.Context(), ddl); err != nil {
		t.Fatal(err)
	}

	return sqlc.New(conn)
}
