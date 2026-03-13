package monitor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/timestamp"
)

type detachedSessionCall struct {
	Name    string
	Command string
}

type fakeTmux struct {
	sessions         map[string]bool
	killedSessions   []string
	detachedSessions []detachedSessionCall
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{
		sessions: make(map[string]bool),
	}
}

func (f *fakeTmux) HasSession(_ context.Context, sessionName string) bool {
	return f.sessions[sessionName]
}

func (f *fakeTmux) AttachSession(_ context.Context, _ string) error {
	return nil
}

func (f *fakeTmux) NewSession(_ context.Context, _ string, _ []string, _ string, _ string) error {
	return nil
}

func (f *fakeTmux) ListSessionNames(_ context.Context) ([]string, error) {
	var names []string
	for name := range f.sessions {
		names = append(names, name)
	}
	return names, nil
}

func (f *fakeTmux) KillSession(_ context.Context, sessionName string) error {
	f.killedSessions = append(f.killedSessions, sessionName)
	return nil
}

func (f *fakeTmux) NewDetachedSession(_ context.Context, sessionName string, command string) error {
	f.detachedSessions = append(f.detachedSessions, detachedSessionCall{
		Name:    sessionName,
		Command: command,
	})
	return nil
}

// writeJSONL creates a JSONL file at the Claude project path for the given session.
func writeJSONL(t *testing.T, homeDir, projectPath, sessionID, content string) {
	t.Helper()
	dir := filepath.Join(homeDir, ".claude", "projects", projectPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestSync(t *testing.T) {
	t.Parallel()

	t.Run("dead session is deleted", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := newFakeTmux()
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		sessions, err := queries.ListSessions(ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sessions) != 0 {
			t.Errorf("expected dead session to be deleted, got %d sessions", len(sessions))
		}
	})

	t.Run("waiting becomes running when JSONL timestamp postdates updated_at", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Write JSONL with a timestamp far in the future, guaranteeing it postdates updated_at.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n")

		// Sync: JSONL max timestamp postdates updated_at, transitions to running
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running", got)
		}
	})

	t.Run("no re-transition when already running", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Write JSONL with future timestamp to postdate updated_at
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n")

		// First sync: transitions to running
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		// Second sync: status is already running, the st == status.Waiting guard prevents re-triggering
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("after second sync: got %q, want running", got)
		}
	})

	t.Run("no transition for non-waiting status", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "stopped", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// First sync: baseline
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		// Write JSONL with a new UUID
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2025-01-01T00:00:01Z"}`+"\n")

		// Second sync: new UUID but status is stopped, should not transition
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped", got)
		}
	})

	t.Run("missing JSONL file is skipped", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Sync without JSONL file: should not error or transition
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting", got)
		}
	})

	t.Run("empty agent_session_id skips JSONL processing", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		// No agent_session_id set

		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting", got)
		}
	})

	t.Run("handles JSONL lines exceeding 64KB", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@dev@bigproj": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/dev/bigproj", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-big", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/dev/bigproj",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/dev/bigproj",
		}); err != nil {
			t.Fatal(err)
		}

		// First sync: baseline with no JSONL
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		// Create a JSONL file where the first line exceeds 64KB,
		// followed by a line with a new UUID. Use future timestamps
		// so they postdate the session's updated_at.
		padding := strings.Repeat("x", 70*1024)
		largeLine := `{"uuid":"uuid-big","timestamp":"2099-01-01T00:00:01.000Z","padding":"` + padding + `"}` + "\n"
		normalLine := `{"uuid":"uuid-after","timestamp":"2099-01-01T00:00:02.000Z"}` + "\n"
		writeJSONL(t, homeDir, "-home-dev-bigproj", "sess-big", largeLine+normalLine)

		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/dev/bigproj",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (scanner must handle >64KB lines)", got)
		}
	})

	t.Run("old timestamps predating waiting do not trigger transition", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		// Session is waiting (updated_at = now, guaranteed > 2000-...)
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Write JSONL entries with timestamps in the past (before waiting was set)
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-old","timestamp":"2000-01-01T00:00:01.000Z"}`+"\n"+
				`{"uuid":"uuid-new-but-old-ts","timestamp":"2000-01-01T00:00:02.000Z"}`+"\n")

		// Sync: max timestamp predates updated_at, no transition
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (old JSONL entries should not trigger transition)", got)
		}
	})

	t.Run("new UUIDs postdating waiting trigger transition", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		// Session starts as running
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// First sync: baseline with no JSONL
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		// Simulate hook setting status to waiting
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// Write JSONL entry with a far-future timestamp (postdates updated_at)
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-future","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n")

		// Second sync: new UUID with future timestamp should transition to running
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (future JSONL entries should trigger transition)", got)
		}
	})

	t.Run("waiting becomes running even when entries were already scanned", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		// Session starts as running
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Step 1: Write JSONL entries and sync while session is running.
		// These entries are scanned and would have been recorded into the DB in the old implementation.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2000-01-01T00:00:01.000Z"}`+"\n")
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		// Step 2: PermissionRequest hook fires → status = waiting, updated_at = now
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// Step 3: User approves → Claude writes a new JSONL entry with a future timestamp
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2000-01-01T00:00:01.000Z"}`+"\n"+
				`{"uuid":"uuid-2","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n")

		// Step 4: Sync again — should detect the new entry's timestamp postdates updated_at
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (new entry after approval should trigger transition)", got)
		}
	})

	t.Run("running becomes stopped on interruption", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n"+
				`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user at 2099-01-01]"}]},"timestamp":"2099-01-01T00:00:02.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped", got)
		}
	})

	t.Run("waiting becomes stopped on interruption", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user at 2099-01-01]"}]},"timestamp":"2099-01-01T00:00:01.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped", got)
		}
	})

	t.Run("interruption with old timestamp is ignored", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user at 2000-01-01]"}]},"timestamp":"2000-01-01T00:00:01.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (old interruption should be ignored)", got)
		}
	})

	t.Run("interruption not re-triggered after new prompt", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Use a near-future timestamp so it exceeds the initial updated_at
		// but will be less than the updated_at set by the hook below.
		time.Sleep(10 * time.Millisecond)
		interruptTS := time.Now().UTC().Format(timestamp.Format)
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]},"timestamp":"`+interruptTS+`"}`+"\n")

		// First sync: transitions to stopped
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Fatalf("after first sync: got %q, want stopped", got)
		}

		// Simulate hook setting status to running (advances updated_at beyond the interruption timestamp)
		time.Sleep(10 * time.Millisecond)
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// Second sync with same JSONL: should not re-trigger stop
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		got, err = queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("after second sync: got %q, want running (should not re-trigger stop)", got)
		}
	})

	t.Run("interruption in middle of file is ignored", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Interruption line followed by a normal line
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user at 2099-01-01]"}]},"timestamp":"2099-01-01T00:00:01.000Z"}`+"\n"+
				`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:02.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (interruption in middle should be ignored)", got)
		}
	})

	t.Run("interruption takes priority over waiting to running", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "claude", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// First sync: baseline with no JSONL
		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		// JSONL has a new UUID then an interruption as the last line
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n"+
				`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user at 2099-01-01]"}]},"timestamp":"2099-01-01T00:00:02.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped (interruption should take priority over waiting->running)", got)
		}
	})

	t.Run("unknown tool skips JSONL processing", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		// No UpdateAgentTool call — agent_tool remains empty (unknown)

		// Write JSONL at the Claude path — monitor should NOT read it
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (unknown tool should skip JSONL processing)", got)
		}
	})

	t.Run("CAS protects against concurrent hook update", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		queries := database.SetupTestDB(t)

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Simulate: monitor reads session as "waiting", then hook changes to "stopped"
		// before monitor's CAS write
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "stopped", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// CAS should be a no-op because status is now "stopped", not "waiting"
		if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
			Status:    "running",
			UpdatedAt: timestamp.Now(),
			Name:      "default",
			Path:      "/home/user/project",
			Status_2:  "waiting",
		}); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped (CAS should be no-op)", got)
		}
	})
}

func TestSync_WritesHeartbeat(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := newFakeTmux()
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	if err := sync(ctx, ft, queries, homeDir, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	row, err := queries.GetMonitorHeartbeat(ctx)
	if err != nil {
		t.Fatalf("expected heartbeat record, got error: %v", err)
	}
	if row.UpdatedAt == "" {
		t.Error("expected non-empty heartbeat timestamp")
	}
}

func TestRunCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	ft := newFakeTmux()
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, ft, queries, homeDir, t.TempDir(), discardLogger)
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil on cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// writeCodexLog creates a Codex session log file at the well-known path.
func writeCodexLog(t *testing.T, cacheDir, content string) {
	t.Helper()
	const tmuxName = "muxac-default@home@user@project"
	dir := filepath.Join(cacheDir, "codex", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tmuxName+".jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSyncCodex(t *testing.T) {
	t.Parallel()

	t.Run("task_started transitions to running", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "stopped", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"task_started","turn_id":"t1"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running", got)
		}
	})

	t.Run("task_complete transitions to stopped", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"task_complete","turn_id":"t1"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped", got)
		}
	})

	t.Run("exec_approval_request transitions to waiting", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"exec_approval_request","call_id":"c1","command":"ls"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting", got)
		}
	})

	t.Run("apply_patch_approval_request transitions to waiting", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"apply_patch_approval_request","call_id":"c1","turn_id":"t1"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting", got)
		}
	})

	t.Run("request_permissions transitions to waiting", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"request_permissions"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting", got)
		}
	})

	t.Run("request_user_input transitions to waiting", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"request_user_input"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting", got)
		}
	})

	t.Run("op user_input transitions to running", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"from_tui","kind":"op","payload":{"type":"user_input","text":"hello"}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running", got)
		}
	})

	t.Run("op user_turn transitions to running", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"from_tui","kind":"op","payload":{"type":"user_turn"}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running", got)
		}
	})

	t.Run("turn_aborted transitions to stopped", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"turn_aborted","turn_id":"t1"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped", got)
		}
	})

	t.Run("shutdown_complete transitions to stopped", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"shutdown_complete"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped", got)
		}
	})

	t.Run("interrupt during waiting results in stopped via turn_aborted", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Simulates: approval request → user Ctrl+C → exec_approval (abort) → turn_aborted
		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"exec_approval_request","call_id":"c1","command":"ls"}}}`+"\n"+
				`{"ts":"2099-01-01T00:00:02.000Z","dir":"from_tui","kind":"op","payload":{"type":"exec_approval","id":"c1","decision":"abort"}}`+"\n"+
				`{"ts":"2099-01-01T00:00:03.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"turn_aborted","turn_id":"t1"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped (interrupt during waiting should result in stopped)", got)
		}
	})

	t.Run("op exec_approval approved transitions to running", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"exec_approval_request","call_id":"c1","command":"ls"}}}`+"\n"+
				`{"ts":"2099-01-01T00:00:02.000Z","dir":"from_tui","kind":"op","payload":{"type":"exec_approval","id":"c1","decision":"approved"}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (approval should transition to running)", got)
		}
	})

	t.Run("op exec_approval abort transitions to stopped", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"exec_approval_request","call_id":"c1","command":"ls"}}}`+"\n"+
				`{"ts":"2099-01-01T00:00:02.000Z","dir":"from_tui","kind":"op","payload":{"type":"exec_approval","id":"c1","decision":"abort"}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped (abort should transition to stopped)", got)
		}
	})

	t.Run("op interrupt transitions to stopped", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"from_tui","kind":"op","payload":{"type":"interrupt"}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped", got)
		}
	})

	t.Run("session_end transitions to stopped", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"meta","kind":"session_end"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped", got)
		}
	})

	t.Run("old event timestamp is ignored", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "stopped", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2000-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"task_started","turn_id":"t1"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "stopped" {
			t.Errorf("got %q, want stopped (old event should be ignored)", got)
		}
	})

	t.Run("last event wins with multiple events", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "stopped", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"task_started","turn_id":"t1"}}}`+"\n"+
				`{"ts":"2099-01-01T00:00:02.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"exec_approval_request","call_id":"c1","command":"ls"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (last event should win)", got)
		}
	})

	t.Run("codex log file cleaned up when tmux session dies", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := newFakeTmux()
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"task_started","turn_id":"t1"}}}`+"\n")

		logPath := filepath.Join(cacheDir, "codex", "sessions", "muxac-default@home@user@project.jsonl")
		if _, err := os.Stat(logPath); err != nil {
			t.Fatalf("codex log should exist before sync: %v", err)
		}

		// tmux session is dead (not in ft.sessions)
		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(logPath); !os.IsNotExist(err) {
			t.Errorf("codex log file should be cleaned up when tmux session dies")
		}
	})

	t.Run("no codex log file falls through to claude logic", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		// No agent_tool set, no codex log → should not error
		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting", got)
		}
	})

	t.Run("auto-detection by file existence", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		cacheDir := t.TempDir()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "stopped", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		// No agent_tool set, but codex log file exists → should auto-detect as Codex
		writeCodexLog(t, cacheDir,
			`{"ts":"2099-01-01T00:00:01.000Z","dir":"to_tui","kind":"codex_event","payload":{"id":"sub-1","msg":{"type":"task_started","turn_id":"t1"}}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, cacheDir); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (auto-detected Codex)", got)
		}

		// Verify agent_tool was set in DB
		sessions, err := queries.ListSessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 1 || sessions[0].AgentTool != "codex" {
			t.Errorf("agent_tool = %q, want codex", sessions[0].AgentTool)
		}
	})

	t.Run("CAS protects against concurrent updates", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		queries := database.SetupTestDB(t)

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// Simulate: monitor reads session as "running", then hook changes to "waiting"
		// before monitor's CAS write
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// CAS should be a no-op because status is now "waiting", not "running"
		if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
			Status:    "stopped",
			UpdatedAt: timestamp.Now(),
			Name:      "default",
			Path:      "/home/user/project",
			Status_2:  "running",
		}); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (CAS should be no-op)", got)
		}
	})
}
