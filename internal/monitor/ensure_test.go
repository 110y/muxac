package monitor

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/timestamp"
	"github.com/110y/muxac/internal/version"
)

func TestEnsureRunning_NoSessionNoHeartbeat(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := newFakeTmux()
	queries := database.SetupTestDB(t)

	err := EnsureRunning(ctx, ft, queries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ft.killedSessions) != 0 {
		t.Errorf("expected no kill calls, got %v", ft.killedSessions)
	}
	if len(ft.detachedSessions) != 1 {
		t.Fatalf("expected 1 detached session, got %d", len(ft.detachedSessions))
	}
	if ft.detachedSessions[0].Name != monitorSessionName {
		t.Errorf("session name = %q, want %q", ft.detachedSessions[0].Name, monitorSessionName)
	}
}

func TestEnsureRunning_SessionExistsFreshHeartbeat(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := newFakeTmux()
	ft.sessions[monitorSessionName] = true
	queries := database.SetupTestDB(t)

	// Write a fresh heartbeat with matching version.
	if err := queries.UpsertMonitorHeartbeat(ctx, sqlc.UpsertMonitorHeartbeatParams{
		Version:   version.Version,
		UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	err := EnsureRunning(ctx, ft, queries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ft.killedSessions) != 0 {
		t.Errorf("expected no kill calls, got %v", ft.killedSessions)
	}
	if len(ft.detachedSessions) != 0 {
		t.Errorf("expected no detached sessions, got %v", ft.detachedSessions)
	}
}

func TestEnsureRunning_SessionExistsFreshHeartbeatVersionMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := newFakeTmux()
	ft.sessions[monitorSessionName] = true
	queries := database.SetupTestDB(t)

	// Write a fresh heartbeat with a different version.
	if err := queries.UpsertMonitorHeartbeat(ctx, sqlc.UpsertMonitorHeartbeatParams{
		Version:   "old-version",
		UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	err := EnsureRunning(ctx, ft, queries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ft.killedSessions) != 1 {
		t.Fatalf("expected 1 kill call, got %d", len(ft.killedSessions))
	}
	if ft.killedSessions[0] != monitorSessionName {
		t.Errorf("killed session = %q, want %q", ft.killedSessions[0], monitorSessionName)
	}
	if len(ft.detachedSessions) != 1 {
		t.Fatalf("expected 1 detached session, got %d", len(ft.detachedSessions))
	}
}

func TestEnsureRunning_SessionExistsStaleHeartbeat(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := newFakeTmux()
	ft.sessions[monitorSessionName] = true

	// Insert a stale heartbeat (20 seconds ago).
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	ddl, err := database.LoadDDL()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	// Use a raw query to insert a stale timestamp.
	staleTS := time.Now().Add(-20 * time.Second).UTC().Format(timestamp.Format)
	_, err = conn.ExecContext(ctx,
		"INSERT INTO monitor_heartbeat (id, version, updated_at) VALUES (1, ?, ?)",
		version.Version, staleTS)
	if err != nil {
		t.Fatal(err)
	}
	queries := sqlc.New(conn)

	err = EnsureRunning(ctx, ft, queries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ft.killedSessions) != 1 {
		t.Fatalf("expected 1 kill call, got %d", len(ft.killedSessions))
	}
	if ft.killedSessions[0] != monitorSessionName {
		t.Errorf("killed session = %q, want %q", ft.killedSessions[0], monitorSessionName)
	}
	if len(ft.detachedSessions) != 1 {
		t.Fatalf("expected 1 detached session, got %d", len(ft.detachedSessions))
	}
}

func TestEnsureRunning_SessionExistsNoHeartbeatRow(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := newFakeTmux()
	ft.sessions[monitorSessionName] = true
	queries := database.SetupTestDB(t)

	// No heartbeat row inserted.

	err := EnsureRunning(ctx, ft, queries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ft.killedSessions) != 1 {
		t.Fatalf("expected 1 kill call, got %d", len(ft.killedSessions))
	}
	if ft.killedSessions[0] != monitorSessionName {
		t.Errorf("killed session = %q, want %q", ft.killedSessions[0], monitorSessionName)
	}
	if len(ft.detachedSessions) != 1 {
		t.Fatalf("expected 1 detached session, got %d", len(ft.detachedSessions))
	}
}
