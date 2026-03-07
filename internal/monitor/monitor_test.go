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

		if err := sync(ctx, ft, queries, homeDir); err != nil {
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

	t.Run("waiting becomes running via UUID change", func(t *testing.T) {
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

		// First sync: no JSONL file yet, no UUID recorded
		if err := sync(ctx, ft, queries, homeDir); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Fatalf("after first sync: got %q, want waiting", got)
		}

		// Write JSONL with a new UUID whose timestamp is far in the future,
		// guaranteeing it postdates the session's updated_at.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n")

		// Second sync: new UUID detected with future timestamp, transitions to running
		if err := sync(ctx, ft, queries, homeDir); err != nil {
			t.Fatal(err)
		}
		got, err = queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("after second sync: got %q, want running", got)
		}
	})

	t.Run("no transition when no new UUIDs", func(t *testing.T) {
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

		// Write JSONL with initial UUID (future timestamp to postdate updated_at)
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n")

		// First sync: records UUID and transitions to running
		if err := sync(ctx, ft, queries, homeDir); err != nil {
			t.Fatal(err)
		}

		// Second sync: same JSONL content, no new UUID
		if err := sync(ctx, ft, queries, homeDir); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		// The first sync transitioned to running (prevUUID="" -> currentUUID="uuid-1"),
		// but the second sync should not change it further since no new UUIDs
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
		if err := sync(ctx, ft, queries, homeDir); err != nil {
			t.Fatal(err)
		}

		// Write JSONL with a new UUID
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2025-01-01T00:00:01Z"}`+"\n")

		// Second sync: new UUID but status is stopped, should not transition
		if err := sync(ctx, ft, queries, homeDir); err != nil {
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
		if err := sync(ctx, ft, queries, homeDir); err != nil {
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

		if err := sync(ctx, ft, queries, homeDir); err != nil {
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
		if err := sync(ctx, ft, queries, homeDir); err != nil {
			t.Fatal(err)
		}

		// Create a JSONL file where the first line exceeds 64KB,
		// followed by a line with a new UUID. Use future timestamps
		// so they postdate the session's updated_at.
		padding := strings.Repeat("x", 70*1024)
		largeLine := `{"uuid":"uuid-big","timestamp":"2099-01-01T00:00:01.000Z","padding":"` + padding + `"}` + "\n"
		normalLine := `{"uuid":"uuid-after","timestamp":"2099-01-01T00:00:02.000Z"}` + "\n"
		writeJSONL(t, homeDir, "-home-dev-bigproj", "sess-big", largeLine+normalLine)

		if err := sync(ctx, ft, queries, homeDir); err != nil {
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

	t.Run("old UUIDs predating waiting do not trigger transition", func(t *testing.T) {
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

		// Write JSONL entries with timestamps in the past (before waiting was set)
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-old","timestamp":"2000-01-01T00:00:01.000Z"}`+"\n")

		// First sync: records uuid-old while session is running
		if err := sync(ctx, ft, queries, homeDir); err != nil {
			t.Fatal(err)
		}

		// Simulate hook setting status to waiting (updated_at = now, guaranteed > 2000-...)
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// Write another JSONL entry that also predates the waiting transition
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-old","timestamp":"2000-01-01T00:00:01.000Z"}`+"\n"+
				`{"uuid":"uuid-new-but-old-ts","timestamp":"2000-01-01T00:00:02.000Z"}`+"\n")

		// Second sync: finds new UUID but its timestamp predates updated_at
		if err := sync(ctx, ft, queries, homeDir); err != nil {
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
		if err := sync(ctx, ft, queries, homeDir); err != nil {
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
		if err := sync(ctx, ft, queries, homeDir); err != nil {
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

		if err := sync(ctx, ft, queries, homeDir); err != nil {
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

		if err := sync(ctx, ft, queries, homeDir); err != nil {
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

		if err := sync(ctx, ft, queries, homeDir); err != nil {
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
		if err := sync(ctx, ft, queries, homeDir); err != nil {
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
		if err := sync(ctx, ft, queries, homeDir); err != nil {
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

		if err := sync(ctx, ft, queries, homeDir); err != nil {
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
		if err := sync(ctx, ft, queries, homeDir); err != nil {
			t.Fatal(err)
		}

		// JSONL has a new UUID then an interruption as the last line
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n"+
				`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user at 2099-01-01]"}]},"timestamp":"2099-01-01T00:00:02.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir); err != nil {
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

		if err := sync(ctx, ft, queries, homeDir); err != nil {
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

	if err := sync(ctx, ft, queries, homeDir); err != nil {
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
		done <- Run(ctx, ft, queries, homeDir, discardLogger)
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
