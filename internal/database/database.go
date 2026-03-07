package database

//go:generate ../../bin/sqlc generate -f ../../sqlc.yaml

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"

	"github.com/110y/muxac/internal/database/sqlc"
)

// Open opens (or creates) the SQLite database and returns ready-to-use Queries and the underlying connection.
func Open(ctx context.Context, ddl string) (*sqlc.Queries, *sql.DB, error) {
	dbFile := dbPath()

	if err := os.MkdirAll(filepath.Dir(dbFile), 0o755); err != nil {
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

	if _, err := conn.ExecContext(ctx, ddl); err != nil {
		conn.Close()
		return nil, nil, err
	}

	// Migrate: drop old-schema table so DDL recreates it with new columns.
	var hasOldSchema int
	row := conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'path_key'")
	if err := row.Scan(&hasOldSchema); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if hasOldSchema > 0 {
		if _, err := conn.ExecContext(ctx, "DROP TABLE sessions"); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}

	// Migrate: replace checksum-based schema with agent_session_id, preserving existing rows.
	var hasChecksumCol int
	row = conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'content_checksum'")
	if err := row.Scan(&hasChecksumCol); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if hasChecksumCol > 0 {
		if _, err := conn.ExecContext(ctx, `CREATE TABLE sessions_new (
			name               TEXT NOT NULL,
			path               TEXT NOT NULL,
			status             TEXT NOT NULL CHECK (status IN ('running', 'waiting', 'stopped', 'unknown')),
			agent_session_id   TEXT NOT NULL DEFAULT '',
			created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
			PRIMARY KEY (name, path)
		)`); err != nil {
			conn.Close()
			return nil, nil, err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO sessions_new (name, path, status, created_at, updated_at)
			SELECT name, path, status, created_at, updated_at FROM sessions`); err != nil {
			conn.Close()
			return nil, nil, err
		}
		if _, err := conn.ExecContext(ctx, `DROP TABLE sessions`); err != nil {
			conn.Close()
			return nil, nil, err
		}
		if _, err := conn.ExecContext(ctx, `ALTER TABLE sessions_new RENAME TO sessions`); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}

	// Migrate: rename claude_session_id → agent_session_id.
	var hasClaudeCol int
	row = conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'claude_session_id'")
	if err := row.Scan(&hasClaudeCol); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if hasClaudeCol > 0 {
		if _, err := conn.ExecContext(ctx,
			"ALTER TABLE sessions RENAME COLUMN claude_session_id TO agent_session_id"); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}

	// Migrate: add version column to monitor_heartbeat.
	var hasVersionCol int
	row = conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info('monitor_heartbeat') WHERE name = 'version'")
	if err := row.Scan(&hasVersionCol); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if hasVersionCol == 0 {
		if _, err := conn.ExecContext(ctx,
			"ALTER TABLE monitor_heartbeat ADD COLUMN version TEXT NOT NULL DEFAULT ''"); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}

	// Migrate: add agent_tool column to sessions.
	var hasAgentToolCol int
	row = conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'agent_tool'")
	if err := row.Scan(&hasAgentToolCol); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if hasAgentToolCol == 0 {
		if _, err := conn.ExecContext(ctx,
			"ALTER TABLE sessions ADD COLUMN agent_tool TEXT NOT NULL DEFAULT ''"); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}

	return sqlc.New(conn), conn, nil
}

func dbPath() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(dir, "muxac", "db")
}
