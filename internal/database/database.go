package database

//go:generate ../../bin/sqlc generate -f ../../sqlc.yaml

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/110y/muxac/internal/database/sqlc"
)

// Open opens (or creates) the SQLite database and returns ready-to-use Queries and the underlying connection.
// cacheDir is the muxac cache directory (e.g. ~/.cache/muxac); the database file is stored at cacheDir/db.
// migrations is an fs.FS containing SQL migration files (e.g. from go:embed).
func Open(ctx context.Context, migrations fs.FS, cacheDir string) (*sqlc.Queries, *sql.DB, error) {
	dbFile := filepath.Join(cacheDir, "db")

	if err := os.MkdirAll(filepath.Dir(dbFile), 0o755); err != nil {
		return nil, nil, err
	}

	if err := resetIfNeeded(ctx, dbFile); err != nil {
		return nil, nil, err
	}

	conn, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return nil, nil, err
	}

	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}

	if err := migrate(ctx, conn, migrations); err != nil {
		conn.Close()
		return nil, nil, err
	}

	return sqlc.New(conn), conn, nil
}

func resetIfNeeded(ctx context.Context, dbFile string) error {
	if _, err := os.Stat(dbFile); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	conn, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return err
	}

	var count int
	err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_migrations'").Scan(&count)
	conn.Close()
	if err != nil {
		return err
	}

	if count > 0 {
		return nil
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(dbFile + suffix)
	}

	return nil
}

func migrate(ctx context.Context, conn *sql.DB, migrations fs.FS) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _migrations (
		version TEXT NOT NULL PRIMARY KEY
	)`); err != nil {
		return err
	}

	files, err := collectMigrations(migrations)
	if err != nil {
		return err
	}

	for _, f := range files {
		applied, err := isMigrationApplied(ctx, conn, f.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		content, err := fs.ReadFile(migrations, f.path)
		if err != nil {
			return err
		}

		if err := applyMigration(ctx, conn, f, content); err != nil {
			return err
		}
	}

	return nil
}

func applyMigration(ctx context.Context, conn *sql.DB, f migrationFile, content []byte) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	if _, err := tx.ExecContext(ctx, string(content)); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, "INSERT INTO _migrations (version) VALUES (?)", f.version); err != nil {
		return err
	}

	return tx.Commit()
}

type migrationFile struct {
	version string
	path    string
}

func collectMigrations(migrations fs.FS) ([]migrationFile, error) {
	var files []migrationFile

	err := fs.WalkDir(migrations, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".sql") {
			return nil
		}
		name := filepath.Base(path)
		version := strings.TrimSuffix(name, ".sql")
		files = append(files, migrationFile{version: version, path: path})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].version < files[j].version
	})

	return files, nil
}

func isMigrationApplied(ctx context.Context, conn *sql.DB, version string) (bool, error) {
	var count int
	err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM _migrations WHERE version = ?", version).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
