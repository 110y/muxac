package database

//go:generate ../../bin/sqlc generate -f ../../sqlc.yaml

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/110y/muxac/internal/database/sqlc"
)

// busyTimeoutPragma makes the queries of a connection wait, rather than fail,
// while the database is locked by another process, as every muxac command and
// the server of every session use it. It takes effect before the next
// statement of the connection: a connection that ran a statement without it
// may have failed with SQLITE_BUSY, as when the last connection to the
// database closes and checkpoints it.
const busyTimeoutPragma = "PRAGMA busy_timeout = 5000"

// Open opens (or creates) the SQLite database and returns ready-to-use Queries and the underlying connection.
// cacheDir is the muxac cache directory (e.g. ~/.cache/muxac); the database file is stored at cacheDir/sessions.db.
// migrations is an fs.FS containing SQL migration files (e.g. from go:embed).
func Open(ctx context.Context, migrations fs.FS, cacheDir string) (*sqlc.Queries, *sql.DB, error) {
	// Versions of muxac that ran their sessions in tmux keep their database
	// in cacheDir/db. A monitor such a version left running in tmux deletes
	// the row of every session it does not find there, so it must not share
	// the database of the sessions muxac runs itself.
	dbFile := filepath.Join(cacheDir, "sessions.db")

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
	// The pragmas below but journal_mode only apply to the connection they
	// are run on. With a single connection, every query runs with them, even
	// in the processes that query from several goroutines for long, such as
	// the server of a session.
	conn.SetMaxOpenConns(1)

	pragmas := []string{
		// First, since the others may have to wait as well.
		busyTimeoutPragma,
		"PRAGMA journal_mode = WAL",
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
	// The query waits as the ones of Open do: on the same connection as the
	// pragma.
	conn.SetMaxOpenConns(1)

	var count int
	_, err = conn.ExecContext(ctx, busyTimeoutPragma)
	if err == nil {
		err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='_migrations'").Scan(&count)
	}
	if err := errors.Join(err, conn.Close()); err != nil {
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
	// Disable foreign keys before the transaction so that migrations can
	// drop and recreate tables referenced by foreign keys.
	// PRAGMA foreign_keys is a no-op inside a transaction.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "PRAGMA foreign_keys = ON") //nolint:errcheck // best-effort re-enable

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
