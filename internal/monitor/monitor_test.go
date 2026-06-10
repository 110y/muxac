package monitor

import (
	"context"
	"fmt"
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
	sessions           map[string]bool
	killedSessions     []string
	detachedSessions   []detachedSessionCall
	capturePaneOutputs map[string]string
	capturePaneErr     error
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

func (f *fakeTmux) CapturePane(_ context.Context, sessionName string) (string, error) {
	if f.capturePaneErr != nil {
		return "", f.capturePaneErr
	}
	return f.capturePaneOutputs[sessionName], nil
}

func newMonitorState() *monitorState {
	return &monitorState{
		capturePaneClearCount:      make(map[string]int),
		capturePromptSeen:          make(map[string]bool),
		captureRunningClearCount:   make(map[string]int),
		captureRunningWaitingCount: make(map[string]int),
	}
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

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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

	t.Run("waiting becomes running when JSONL timestamp exceeds waiting_since threshold", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		// WaitingSince is set to a known past time; JSONL entry is >2s after it.
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: "2090-01-01T00:00:00.000Z", WaitingSince: "2090-01-01T00:00:00.000Z",
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

		// JSONL entry timestamp is >2s after WaitingSince → should transition.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2090-01-01T00:00:05.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: "2090-01-01T00:00:00.000Z", WaitingSince: "2090-01-01T00:00:00.000Z",
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

		state := newMonitorState()

		// JSONL entry >2s after WaitingSince → transitions to running.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2090-01-01T00:00:05.000Z"}`+"\n")

		// First sync: transitions to running
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// Second sync: status is already running, the st == status.Waiting guard prevents re-triggering
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
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
			Name: "default", Path: "/home/user/project", Status: "idle", UpdatedAt: timestamp.Now(),
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
		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		// Write JSONL with a new UUID
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2025-01-01T00:00:01Z"}`+"\n")

		// Second sync: new UUID but status is idle, should not transition
		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle", got)
		}
	})

	t.Run("missing JSONL file is skipped", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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
		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		// No agent_session_id set

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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
			Name: "default", Path: "/home/dev/bigproj", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		state := newMonitorState()

		// Write initial small JSONL to establish baseline.
		writeJSONL(t, homeDir, "-home-dev-bigproj", "sess-big",
			`{"uuid":"uuid-baseline","timestamp":"2098-01-01T00:00:01.000Z"}`+"\n")

		// First sync: records baseline.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// Create a JSONL file where the first line exceeds 64KB,
		// followed by a line with a new UUID. Use future timestamps
		// so they postdate the baseline.
		padding := strings.Repeat("x", 70*1024)
		largeLine := `{"uuid":"uuid-big","timestamp":"2099-01-01T00:00:01.000Z","padding":"` + padding + `"}` + "\n"
		normalLine := `{"uuid":"uuid-after","timestamp":"2099-01-01T00:00:02.000Z"}` + "\n"
		writeJSONL(t, homeDir, "-home-dev-bigproj", "sess-big", largeLine+normalLine)

		// Second sync: detects newer entry after baseline, transitions to running.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
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
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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
		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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

	t.Run("JSONL entries within 2s buffer of waiting_since stay waiting", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		// WaitingSince = T, JSONL timestamp = T+1s (within 2s buffer).
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: "2090-01-01T00:00:00.000Z", WaitingSince: "2090-01-01T00:00:00.000Z",
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

		// JSONL entry at T+1s — within the 2s buffer, should NOT trigger transition.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2090-01-01T00:00:01.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (entries within 2s buffer should not trigger transition)", got)
		}
	})

	t.Run("JSONL entries beyond 2s buffer of waiting_since trigger transition", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		// WaitingSince = T, JSONL timestamp = T+3s (beyond 2s buffer).
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: "2090-01-01T00:00:00.000Z", WaitingSince: "2090-01-01T00:00:00.000Z",
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

		// JSONL entry at T+3s — beyond the 2s buffer, should trigger transition.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2090-01-01T00:00:03.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (entries beyond 2s buffer should trigger transition)", got)
		}
	})

	t.Run("new JSONL entries postdating waiting_since threshold trigger transition", func(t *testing.T) {
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

		state := newMonitorState()

		// First sync: session is running, no JSONL
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// Simulate hook setting status to waiting with a known WaitingSince.
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: "2090-01-01T00:00:00.000Z", WaitingSince: "2090-01-01T00:00:00.000Z",
		}); err != nil {
			t.Fatal(err)
		}

		// Write JSONL entry >2s after WaitingSince (represents activity after user approved).
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-future","timestamp":"2090-01-01T00:00:05.000Z"}`+"\n")

		// Sync detects JSONL timestamp exceeding threshold, transitions to running.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (JSONL entries after threshold should trigger transition)", got)
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

		state := newMonitorState()

		// Step 1: Write JSONL entries and sync while session is running.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2000-01-01T00:00:01.000Z"}`+"\n")
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// Step 2: PermissionRequest hook fires → status = waiting with known WaitingSince.
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: "2090-01-01T00:00:00.000Z", WaitingSince: "2090-01-01T00:00:00.000Z",
		}); err != nil {
			t.Fatal(err)
		}

		// Step 3: User approves → Claude writes a new JSONL entry >2s after WaitingSince.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2000-01-01T00:00:01.000Z"}`+"\n"+
				`{"uuid":"uuid-2","timestamp":"2090-01-01T00:00:05.000Z"}`+"\n")

		// Step 4: Sync detects JSONL timestamp exceeding threshold, transitions to running.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
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

	t.Run("running becomes idle on interruption", func(t *testing.T) {
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

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle", got)
		}
	})

	t.Run("running becomes idle on interruption followed by last-prompt marker", func(t *testing.T) {
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

		// Claude Code appends a "last-prompt" marker line after the interruption
		// when the user stops the agent and does not submit a new prompt.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]},"uuid":"intr-1","timestamp":"2099-01-01T00:00:02.000Z"}`+"\n"+
				`{"type":"last-prompt","leafUuid":"intr-1","sessionId":"sess-123"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle", got)
		}
	})

	t.Run("running becomes idle on interruption followed by file-history-snapshot", func(t *testing.T) {
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

		// Claude Code appends a "file-history-snapshot" marker line after an
		// interruption during tool use.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]},"uuid":"intr-1","timestamp":"2099-01-01T00:00:02.000Z"}`+"\n"+
				`{"type":"file-history-snapshot","messageId":"snap-1","snapshot":{"messageId":"snap-1","trackedFileBackups":{},"timestamp":"2099-01-01T00:00:03.000Z"},"isSnapshotUpdate":false}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle", got)
		}
	})

	t.Run("running becomes idle when escaped before any output (no interruption line)", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// Idle terminal: input box plus the user's custom status line. The status
		// line reports context as a percentage and a clock "(3h 57m)", which must
		// NOT be mistaken for the agent's "(12s · ↓ N tokens)" processing readout.
		idlePane := "╭────────────────────────────────────────╮\n" +
			"│ > do the thing                         │\n" +
			"╰────────────────────────────────────────╯\n" +
			"  21% | 5h: 6% (3h 57m) | 7d: 36% (Mon 23:00)        max"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": idlePane},
		}
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

		// Escaped immediately after submitting: the JSONL ends with the user's
		// prompt (string content) and bookkeeping markers — no assistant response
		// and no "[Request interrupted by user]" line.
		// The prompt has string content (not an array of turns), so it is not a
		// conversation line; with the markers it leaves no recent conversation
		// write, so the terminal alone decides the session is idle.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"role":"user","content":"do the thing"},"uuid":"p1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n"+
				`{"type":"last-prompt","lastPrompt":"do the thing","leafUuid":"p1","sessionId":"sess-123"}`+"\n"+
				`{"type":"mode","mode":"normal","sessionId":"sess-123"}`+"\n")

		state := newMonitorState()

		// The flip requires a sustained idle window (captureRunningIdleThreshold
		// consecutive observations) so that brief no-spinner gaps mid-turn do not
		// flicker a running session to idle; every tick before the threshold stays
		// running.
		for i := 1; i < captureRunningIdleThreshold; i++ {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
			got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
			if err != nil {
				t.Fatal(err)
			}
			if got != "running" {
				t.Fatalf("after sync %d: got %q, want running (debounce)", i, got)
			}
		}

		// The final consecutive idle observation crosses the threshold.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle after %d idle observations", got, captureRunningIdleThreshold)
		}
	})

	t.Run("running does not flicker to idle on a brief no-spinner gap with an unreadable transcript", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// The agent is mid-turn but momentarily shows no spinner (a completed tool
		// result just before its next message). Its transcript holds no usable
		// conversation turn — e.g. a freshly restarted session whose file contains
		// only an "ai-title" line — so conversationRecentlyWritten cannot keep it
		// running. A brief gap like this must NOT flip the session to idle; only a
		// sustained idle window may. Without the debounce this flickered to idle
		// after two ticks and back to running on the next hook event.
		idleLookingPane := "● The test finished. Let me read the results.\n" +
			"● All four phases pass:\n" +
			"────────────────────────\n" +
			"❯ \n" +
			"────────────────────────\n" +
			"  9% | 5h: 27% (1h 37m)        Remote Control active"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": idleLookingPane},
		}
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

		// Transcript holds only a bookkeeping "ai-title" line — no conversation
		// turn — so conversationRecentlyWritten is false throughout.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"ai-title","aiTitle":"Configure nvim leader slash window","sessionId":"sess-123"}`+"\n")

		state := newMonitorState()
		// Across a brief gap (fewer ticks than the threshold) it must stay running.
		for range captureRunningIdleThreshold - 1 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (a brief no-spinner gap must not flicker to idle)", got)
		}
	})

	t.Run("running does not flap to idle while transcript is being written", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// Terminal looks idle to the heuristic — e.g. the spinner is scrolled out
		// of view by a multi-line draft in the input box — yet the agent is busy.
		idleLookingPane := "❯ a long draft the user is typing\n" +
			"  second line of the draft\n" +
			"  third line of the draft\n" +
			"────\n" +
			"  41% | 5h: 16% (0h 37m) | 7d: 38% (Mon 23:00)"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": idleLookingPane},
		}
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

		// Transcript was just written (agent actively producing output): a real
		// assistant turn with a current timestamp.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]},"uuid":"a1","timestamp":"`+timestamp.Now()+`"}`+"\n")

		state := newMonitorState()
		for range 4 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (fresh transcript must keep session busy)", got)
		}
	})

	t.Run("running stays running while terminal shows processing", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		busyPane := "● Let me investigate this.\n\n" +
			"✢ Running… (12s · ↓ 3.4k tokens)\n" +
			"────────────────────────────────\n" +
			"❯ \n" +
			"────────────────────────────────\n" +
			"  21% | 5h: 6% (3h 57m)        max"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": busyPane},
		}
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

		state := newMonitorState()
		// Even after several ticks, an actively-processing session is never reverted.
		for range 4 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (agent still processing)", got)
		}
	})

	t.Run("running with empty capture-pane stays running", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// Empty output (transient render) must be treated as busy, not idle.
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": ""},
		}
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

		state := newMonitorState()
		for range 3 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (empty pane treated as busy)", got)
		}
	})

	t.Run("running stays running when a draft hides the status line but esc to interrupt is visible", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// The agent is busy (status line shows "esc to interrupt"), but the user
		// is drafting a multi-line next message, which pushes the status line
		// above the tail window the heuristic inspects. The session must NOT be
		// reverted to idle: "esc to interrupt" is a definitive busy signal and is
		// detected across the whole pane.
		busyDraftPane := "✢ Running… (1m 4s · esc to interrupt)\n" +
			"╭──────────────────────────────╮\n" +
			"│ > draft line 1                │\n" +
			"│ draft line 2                  │\n" +
			"│ draft line 3                  │\n" +
			"│ draft line 4                  │\n" +
			"│ draft line 5                  │\n" +
			"│ draft line 6                  │\n" +
			"╰──────────────────────────────╯\n" +
			"  21% | 5h: 6% (3h 57m)        max"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": busyDraftPane},
		}
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

		// A long-running step (e.g. a slow model response or tool call) left no
		// recent conversation write: the only transcript entries are the user's
		// submitted prompt (string content, which does not parse as a turn) and
		// bookkeeping markers. So the terminal alone decides whether it is busy.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"role":"user","content":"do the thing"},"uuid":"p1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n"+
				`{"type":"last-prompt","lastPrompt":"do the thing","leafUuid":"p1","sessionId":"sess-123"}`+"\n"+
				`{"type":"mode","mode":"normal","sessionId":"sess-123"}`+"\n")

		state := newMonitorState()
		for range 4 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (busy session must not flap to idle when the status line is hidden)", got)
		}
	})

	t.Run("running stays running when a draft hides the status line (token readout, no esc-to-interrupt)", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// Same as above, but for Claude versions whose status line shows the
		// "↓ N tokens" readout and no "esc to interrupt" hint. After approving a
		// request the agent works on a long step while the user drafts the next
		// message; the draft scrolls "Running… (… · ↓ N tokens)" above the tail
		// window. The token readout is matched across the whole pane, so the
		// session must stay running instead of momentarily flapping to idle.
		busyDraftPane := "● analysing the change\n" +
			"  ⎿  Listing 1 directory…\n" +
			"✢ Running… (6m 48s · ↓ 29.8k tokens)\n" +
			"────────────────────────────────────────\n" +
			"❯ also update the docs\n" +
			"  and add a test for\n" +
			"  the new behaviour\n" +
			"  then run the linter\n" +
			"  and push the branch\n" +
			"────────────────────────────────────────\n" +
			"  25% | 5h: 8% (4h 24m) | 7d: 6% (Mon 23:00)        Remote Control active"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": busyDraftPane},
		}
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

		// No recent conversation write: only the submitted prompt (string content,
		// not a parseable turn) and bookkeeping markers, so the terminal alone
		// decides whether the session is busy.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"role":"user","content":"do the thing"},"uuid":"p1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n"+
				`{"type":"last-prompt","lastPrompt":"do the thing","leafUuid":"p1","sessionId":"sess-123"}`+"\n")

		state := newMonitorState()
		for range 4 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (token readout must keep a drafting session busy)", got)
		}
	})

	t.Run("running becomes waiting when a permission prompt appears", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// The agent emitted a tool call that needs approval; the permission prompt
		// is on screen, but no hook moved the session to waiting, so muxac still
		// shows running. The monitor must detect the prompt and transition to
		// waiting — even though the tool-call assistant turn was just written
		// (conversationRecentlyWritten), which would otherwise keep it "running".
		promptPane := "● Bash(echo hi)\n" +
			"  ⎿  Waiting…\n" +
			"────────────────────────────────────────\n" +
			" Bash command\n" +
			"   echo hi\n" +
			" Do you want to proceed?\n" +
			" ❯ 1. Yes\n" +
			"   2. No\n" +
			" Esc to cancel · Tab to amend · ctrl+e to explain"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": promptPane},
		}
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

		// A fresh assistant turn (the tool call), so conversationRecentlyWritten is
		// true — the prompt detection must still take priority over it.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"running it"}]},"uuid":"a1","timestamp":"`+timestamp.Now()+`"}`+"\n")

		state := newMonitorState()

		// First sync only arms the debounce; status stays running.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Fatalf("after first sync: got %q, want running (debounce)", got)
		}

		// Second consecutive observation transitions to waiting.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		got, err = queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("after second sync: got %q, want waiting", got)
		}

		// waiting_since must be set so the JSONL waiting→running heuristic works.
		sessions, err := queries.ListSessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 1 || sessions[0].WaitingSince == "" {
			t.Errorf("waiting_since not set on running→waiting transition")
		}
	})

	t.Run("running stays running when no permission prompt is visible", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// Guard against the running→waiting check firing without a real prompt: an
		// ordinary busy terminal must keep the session running across many ticks.
		busyPane := "● working on it\n✢ Running… (12s · ↓ 3.4k tokens)\n────\n❯ \n────\n  21% | 5h: 6% (3h 57m)"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": busyPane},
		}
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

		state := newMonitorState()
		for range 4 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (no prompt visible)", got)
		}
	})

	t.Run("idle becomes waiting directly when a permission prompt appears", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// A permission prompt appeared on a session the hooks left marked idle (a
		// missed/unmapped permission notification). It must be surfaced as waiting
		// directly, not left idle (nor bounced through running).
		promptPane := "● Bash(echo hi)\n" +
			"  ⎿  Waiting…\n" +
			"────────────────────────────────────────\n" +
			" Bash command\n" +
			"   echo hi\n" +
			" Do you want to proceed?\n" +
			" ❯ 1. Yes\n" +
			"   2. No\n" +
			" Esc to cancel · Tab to amend · ctrl+e to explain"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": promptPane},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "idle", UpdatedAt: timestamp.Now(),
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

		state := newMonitorState()

		// First sync arms the debounce; the session must never be seen as running.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Fatalf("after first sync: got %q, want idle (debounce, never running)", got)
		}

		// Second consecutive observation transitions to waiting.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		got, err = queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("after second sync: got %q, want waiting", got)
		}
	})

	t.Run("idle stays idle when the last message merely offers options", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// A finished turn whose closing message offers choices ("Would you like
		// me to…?") is NOT a permission prompt — it has no prompt chrome (no option
		// list, no "esc to cancel" footer). It must stay idle, not be mistaken for
		// a session blocked on a prompt.
		offerPane := "● Done. I fixed the failing test and the lint passes.\n" +
			"● Would you like me to proceed with the refactor, or do you want to review first?\n" +
			"────────────────────────────────────────\n" +
			"❯ \n" +
			"────────────────────────────────────────\n" +
			"  9% | 5h: 27% (1h 37m)        Remote Control active"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": offerPane},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "idle", UpdatedAt: timestamp.Now(),
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

		state := newMonitorState()
		for range 4 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle (an offer of options is not a permission prompt)", got)
		}
	})

	t.Run("waiting becomes idle on interruption", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle", got)
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

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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

		// First sync: transitions to idle
		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Fatalf("after first sync: got %q, want idle", got)
		}

		// Simulate hook setting status to running (advances updated_at beyond the interruption timestamp)
		time.Sleep(10 * time.Millisecond)
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// Second sync with same JSONL: should not re-trigger stop
		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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

		// Interruption line followed by a later conversation turn (a new user
		// prompt): the interruption is no longer the latest conversation line, so
		// it must be ignored even though its timestamp is after updated_at.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user at 2099-01-01]"}]},"timestamp":"2099-01-01T00:00:01.000Z"}`+"\n"+
				`{"type":"user","message":{"content":[{"type":"text","text":"please continue"}]},"timestamp":"2099-01-01T00:00:02.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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
		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		// JSONL has a new UUID then an interruption as the last line
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z"}`+"\n"+
				`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user at 2099-01-01]"}]},"timestamp":"2099-01-01T00:00:02.000Z"}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle (interruption should take priority over waiting->running)", got)
		}
	})

	t.Run("running becomes idle on API error termination", func(t *testing.T) {
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
				`{"type":"assistant","uuid":"uuid-2","timestamp":"2099-01-01T00:00:02.000Z","message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: 529 Overloaded."}]},"isApiErrorMessage":true,"apiErrorStatus":529}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle (API error termination should transition running to idle)", got)
		}
	})

	t.Run("running becomes idle on API error followed by bookkeeping marker", func(t *testing.T) {
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

		// A bookkeeping marker appended after the API error line must not mask it.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"assistant","uuid":"uuid-1","timestamp":"2099-01-01T00:00:02.000Z","message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: 529 Overloaded."}]},"isApiErrorMessage":true,"apiErrorStatus":529}`+"\n"+
				`{"type":"file-history-snapshot","messageId":"snap-1","snapshot":{"messageId":"snap-1","trackedFileBackups":{},"timestamp":"2099-01-01T00:00:03.000Z"},"isSnapshotUpdate":false}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle (API error must be detected despite trailing marker)", got)
		}
	})

	t.Run("waiting becomes idle on API error termination", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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
			`{"type":"assistant","uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z","message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: 529 Overloaded."}]},"isApiErrorMessage":true,"apiErrorStatus":529}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle (API error termination should transition waiting to idle)", got)
		}
	})

	t.Run("API error with old timestamp is ignored", func(t *testing.T) {
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
			`{"type":"assistant","uuid":"uuid-1","timestamp":"2000-01-01T00:00:01.000Z","message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: 529 Overloaded."}]},"isApiErrorMessage":true,"apiErrorStatus":529}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (old API error should be ignored)", got)
		}
	})

	t.Run("API error in middle of file is ignored", func(t *testing.T) {
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

		// API error line followed by a normal assistant line (the agent resumed after the error).
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"assistant","uuid":"uuid-1","timestamp":"2099-01-01T00:00:01.000Z","message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: 529 Overloaded."}]},"isApiErrorMessage":true,"apiErrorStatus":529}`+"\n"+
				`{"type":"assistant","uuid":"uuid-2","timestamp":"2099-01-01T00:00:02.000Z","message":{"model":"claude","content":[{"type":"text","text":"resumed"}]}}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (API error in middle should be ignored)", got)
		}
	})

	t.Run("non-terminal api_error system line does not trigger transition", func(t *testing.T) {
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

		// Intermediate retry log (type=system, subtype=api_error) does not have
		// isApiErrorMessage=true and must not trigger the transition — Claude is still retrying.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"system","subtype":"api_error","level":"error","timestamp":"2099-01-01T00:00:01.000Z","uuid":"uuid-1","retryAttempt":1,"maxRetries":10}`+"\n")

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (intermediate retry log should not transition)", got)
		}
	})

	t.Run("unknown tool skips JSONL processing", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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

	t.Run("waiting becomes running via capture-pane when prompt disappears", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": "  Yes, allow once\n  Yes, allow always\n"},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		// No JSONL file — force capture-pane path
		state := newMonitorState()

		// First sync: prompt is visible, capturePromptSeen is set
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// Prompt disappears and the agent resumes (user accepted): the terminal
		// now shows the live processing status line. Activity is positive evidence
		// of resumption, so the transition to running happens immediately, without
		// waiting out the idle debounce.
		ft.capturePaneOutputs["muxac-default@home@user@project"] = "Some output\n✢ Running… (3s · ↓ 1.2k tokens)\n"

		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("after prompt dismissed with activity: got %q, want running", got)
		}
	})

	t.Run("waiting becomes idle directly when prompt is dismissed without activity", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// While waiting at a permission prompt the user presses Escape. Claude
		// Code fires no usable Stop hook (Waiting+Stop is an invalid transition),
		// and any "[Request interrupted by user for tool use]" line can lag behind
		// the terminal clearing. The terminal is the only timely signal: the prompt
		// is gone and the agent shows no activity, so the session must go straight
		// to idle — it must never bounce through running (waiting → running → idle).
		promptPane := "Some output\n" +
			"  Do you want to proceed?\n" +
			"  Yes, allow once\n" +
			"  Yes, allow always\n" +
			"  No, and tell Claude what to do differently\n"
		idlePane := "╭────────────────────────────────────────╮\n" +
			"│ >                                       │\n" +
			"╰────────────────────────────────────────╯\n" +
			"  21% | 5h: 6% (3h 57m) | 7d: 36% (Mon 23:00)        max"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": promptPane},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		// No JSONL interruption line — the monitor must rely on capture-pane alone.
		state := newMonitorState()

		// First sync: prompt visible, capturePromptSeen is set; stays waiting.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// User presses Escape: the prompt disappears, leaving an idle composer.
		ft.capturePaneOutputs["muxac-default@home@user@project"] = idlePane

		// Across the debounce window the session must never be observed running.
		for i := range 3 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
			got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
			if err != nil {
				t.Fatal(err)
			}
			if got == "running" {
				t.Fatalf("after escape sync %d: got running, want waiting then idle (must not bounce through running)", i+1)
			}
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle", got)
		}
	})

	t.Run("waiting becomes running on approval even when the terminal briefly shows no activity", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		// After approving, a quick tool finishes and the agent is briefly between
		// renders (e.g. model-invocation latency): the terminal shows no "Running…"
		// line yet. The freshly written tool_result is positive evidence the agent
		// resumed, so the session must go to running — never momentarily idle.
		promptPane := "  Do you want to proceed?\n  Yes, allow once\n  Yes, allow always\n"
		quietPane := "╭───────────────╮\n│ >             │\n╰───────────────╯\n  21% | 5h: 6% (3h 57m)        max"
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": promptPane},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		state := newMonitorState()

		// First sync: the permission prompt is visible; capturePromptSeen is set.
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// User approves: the prompt closes and a fresh tool_result lands in the
		// transcript, but the terminal has not re-rendered the processing line yet.
		// (Its timestamp stays within the waiting_since + 2s buffer so the JSONL
		// running heuristic does not fire — the capture-pane path must decide.)
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]},"uuid":"tr1","timestamp":"`+timestamp.Now()+`"}`+"\n")
		ft.capturePaneOutputs["muxac-default@home@user@project"] = quietPane

		// Across several ticks the session must reach running and never be idled.
		for i := range 3 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
			got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
			if err != nil {
				t.Fatal(err)
			}
			if got == "idle" {
				t.Fatalf("after approval sync %d: got idle, want running (fresh tool_result means the agent resumed)", i+1)
			}
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running", got)
		}
	})

	t.Run("waiting stays waiting when prompt is visible", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": "Some output\n  Yes, allow once\n  Yes, allow always\n"},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		state := newMonitorState()
		// Multiple syncs — prompt always visible
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (prompt is visible)", got)
		}
	})

	t.Run("capture-pane debounce resets when prompt reappears", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": "Allow once\nAllow always\n"},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		state := newMonitorState()

		// First sync: prompt visible, capturePromptSeen set
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// Prompt disappears, counter=1
		ft.capturePaneOutputs["muxac-default@home@user@project"] = "Processing...\n"
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// Prompt reappears, counter resets to 0
		ft.capturePaneOutputs["muxac-default@home@user@project"] = "Allow once\nAllow always\n"
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		// Prompt disappears again, counter=1 (reset)
		ft.capturePaneOutputs["muxac-default@home@user@project"] = "Processing...\n"
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (debounce counter should have reset)", got)
		}
	})

	t.Run("capture-pane does not revert waiting when prompt was never seen", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions:           map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{"muxac-default@home@user@project": "Some output\nProcessing files...\n"},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		// No JSONL file — force capture-pane path.
		// Terminal never shows a prompt pattern.
		state := newMonitorState()

		// Multiple syncs: prompt never seen, counter must not increment.
		for i := range 5 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatalf("sync %d: %v", i, err)
			}
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (prompt was never seen, should not revert)", got)
		}
	})

	t.Run("capture-pane error is non-fatal", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions:       map[string]bool{"muxac-default@home@user@project": true},
			capturePaneErr: fmt.Errorf("tmux not responding"),
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
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

		state := newMonitorState()
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (capture-pane error should be non-fatal)", got)
		}
	})

	t.Run("JSONL heuristic blocked when capture-pane shows prompt", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions: map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{
				"muxac-default@home@user@project": "  Yes, allow once\n  Yes, allow always\n",
			},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: "2090-01-01T00:00:00.000Z", WaitingSince: "2090-01-01T00:00:00.000Z",
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

		// Write JSONL entry >2s after WaitingSince.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2090-01-01T00:00:05.000Z"}`+"\n")

		// JSONL heuristic triggers but capture-pane guard sees prompt is visible — status stays waiting.
		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (capture-pane guard should block JSONL heuristic)", got)
		}
	})

	t.Run("JSONL heuristic transitions when capture-pane shows no prompt", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions: map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{
				"muxac-default@home@user@project": "some normal output\n",
			},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: "2090-01-01T00:00:00.000Z", WaitingSince: "2090-01-01T00:00:00.000Z",
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

		// Write JSONL entry >2s after WaitingSince.
		writeJSONL(t, homeDir, "-home-user-project", "sess-123",
			`{"uuid":"uuid-1","timestamp":"2090-01-01T00:00:05.000Z"}`+"\n")

		// JSONL heuristic triggers and capture-pane shows no prompt — transitions to running.
		if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (JSONL heuristic should transition when no prompt visible)", got)
		}
	})

	t.Run("CAS protects against concurrent hook update", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		queries := database.SetupTestDB(t)

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
			AgentSessionID: "sess-123", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		// Simulate: monitor reads session as "waiting", then hook changes to "idle"
		// before monitor's CAS write
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "idle", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// CAS should be a no-op because status is now "idle", not "waiting"
		if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
			Status:       "running",
			UpdatedAt:    timestamp.Now(),
			WaitingSince: "",
			Name:         "default",
			Path:         "/home/user/project",
			Status_2:     "waiting",
		}); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
			Name: "default", Path: "/home/user/project",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle (CAS should be no-op)", got)
		}
	})
}

func TestTerminalShowsWaitingPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"yes allow once", "  Yes, allow once\n", true},
		{"yes allow always", "  Yes, allow always\n", true},
		{"allow once", "  Allow once\n", true},
		{"allow always", "  Allow always\n", true},
		{"no and tell claude", "  No, and tell Claude what to do differently\n", true},
		{"do you want", "Do you want to proceed?\n", true},
		{"would you like", "Would you like to continue?\n", true},
		{"run this command?", "Run this command?\n", true},
		{"execute this?", "Execute this?\n", true},
		{"do you trust the files", "Do you trust the files in this folder?\n", true},
		{"use arrow keys", "Use arrow keys to navigate\n", true},
		{"esc to cancel", "Press Esc to cancel\n", true},
		{"case insensitive", "YES, ALLOW ONCE\n", true},
		{"non-breaking space normalization", "Yes,\u00a0allow\u00a0once\n", true},
		{"pattern outside last 15 lines window", strings.Repeat("unrelated line\n", 20) + "Yes, allow once\n" + strings.Repeat("other line\n", 15), false},
		{"empty output", "", false},
		{"no matching patterns", "Processing files...\nReading data...\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := terminalShowsWaitingPrompt(tt.output)
			if got != tt.want {
				t.Errorf("terminalShowsWaitingPrompt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTerminalShowsPermissionPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		// Real prompts — matched via their chrome (footer / option list).
		{"bash prompt footer", "Do you want to proceed?\n❯ 1. Yes\n  2. No\nEsc to cancel · Tab to amend · ctrl+e to explain", true},
		{"question prompt footer", "  Which approach?\n❯ 1. A\n  2. B\nEnter to select · ↑/↓ to navigate · Esc to cancel", true},
		{"allow once option", "  Yes, allow once\n  Yes, allow always\n", true},
		{"no and tell claude option", "  No, and tell Claude what to do differently\n", true},
		{"folder trust", "Do you trust the files in this folder?\n", true},
		{"arrow keys footer", "Use arrow keys to navigate\n", true},
		{"case insensitive", "ESC TO CANCEL\n", true},
		{"nbsp normalization", "Esc to cancel\n", true},
		// Not prompts — must NOT match, unlike the looser terminalShowsWaitingPrompt.
		{"offer of options in prose", "● Would you like me to proceed, or do you want to review first?\n❯ \n", false},
		{"do-you-want in prose", "I'm not sure what you want here. Do you want me to continue?\n❯ \n", false},
		{"esc to interrupt is the busy hint, not a prompt", "✻ Running… (12s · esc to interrupt)\n❯ \n", false},
		{"empty output", "", false},
		{"ordinary output", "Processing files...\nReading data...\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := terminalShowsPermissionPrompt(tt.output); got != tt.want {
				t.Errorf("terminalShowsPermissionPrompt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSync_WritesHeartbeat(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := newFakeTmux()
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
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

func TestSyncGemini_NoFileSyncNeeded(t *testing.T) {
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
	if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
		AgentTool: "gemini", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}

	if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Status != "running" {
		t.Errorf("status = %q, want %q (should not be changed by file-based sync)", sessions[0].Status, "running")
	}
}

// writeGeminiSession creates a Gemini session JSON file matching the expected glob pattern.
func writeGeminiSession(t *testing.T, homeDir, projectDir, sessionID, content string) {
	t.Helper()
	dir := filepath.Join(homeDir, ".gemini", "tmp", filepath.Base(projectDir), "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	filename := fmt.Sprintf("session-001-%s.json", sessionID[:8])
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSyncGemini_CancellationDetected(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	oldTime := time.Now().Add(-1 * time.Minute).UTC().Format(timestamp.Format)
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: oldTime,
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
		AgentTool: "gemini", UpdatedAt: oldTime, Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
		AgentSessionID: "abcdefgh-1234", UpdatedAt: oldTime, Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}

	cancelTime := time.Now().UTC().Format(time.RFC3339Nano)
	sessionContent := fmt.Sprintf(`{"messages":[{"type":"gemini","timestamp":"%s","content":"some output"},{"type":"info","timestamp":"%s","content":"Request cancelled."}]}`, oldTime, cancelTime)
	writeGeminiSession(t, homeDir, "/home/user/project", "abcdefgh-1234", sessionContent)

	if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Status != "idle" {
		t.Errorf("status = %q, want %q", sessions[0].Status, "idle")
	}
}

func TestSyncGemini_NoCancellation_StatusUnchanged(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	oldTime := time.Now().Add(-1 * time.Minute).UTC().Format(timestamp.Format)
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: oldTime,
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
		AgentTool: "gemini", UpdatedAt: oldTime, Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
		AgentSessionID: "abcdefgh-1234", UpdatedAt: oldTime, Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}

	sessionContent := `{"messages":[{"type":"gemini","timestamp":"2025-01-01T00:00:00Z","content":"some output"}]}`
	writeGeminiSession(t, homeDir, "/home/user/project", "abcdefgh-1234", sessionContent)

	if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Status != "running" {
		t.Errorf("status = %q, want %q", sessions[0].Status, "running")
	}
}

func TestSyncGemini_OldCancellation_StatusUnchanged(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := &fakeTmux{sessions: map[string]bool{"muxac-default@home@user@project": true}}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	recentTime := time.Now().UTC().Format(timestamp.Format)
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: recentTime,
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
		AgentTool: "gemini", UpdatedAt: recentTime, Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
		AgentSessionID: "abcdefgh-1234", UpdatedAt: recentTime, Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}

	oldCancelTime := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano)
	sessionContent := fmt.Sprintf(`{"messages":[{"type":"info","timestamp":"%s","content":"Request cancelled."}]}`, oldCancelTime)
	writeGeminiSession(t, homeDir, "/home/user/project", "abcdefgh-1234", sessionContent)

	if err := sync(ctx, ft, queries, homeDir, newMonitorState()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Status != "running" {
		t.Errorf("status = %q, want %q", sessions[0].Status, "running")
	}
}

func TestSyncGemini_WaitingToRunningViaCapturePaneWhenPromptDismissed(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := &fakeTmux{
		sessions:           map[string]bool{"muxac-default@home@user@project": true},
		capturePaneOutputs: map[string]string{"muxac-default@home@user@project": "Action Required\n? Shell Command  echo hello\nAllow once\nAllow for this session\nNo, suggest changes (esc)\n"},
	}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
		AgentTool: "gemini", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
		AgentSessionID: "abcdefgh-1234", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}

	state := newMonitorState()

	// First sync: prompt is visible, capturePromptSeen is set.
	if err := sync(ctx, ft, queries, homeDir, state); err != nil {
		t.Fatal(err)
	}

	// Prompt disappears (user approved).
	ft.capturePaneOutputs["muxac-default@home@user@project"] = "Processing files...\n"

	// Second sync: prompt gone, counter=1 (debounce).
	if err := sync(ctx, ft, queries, homeDir, state); err != nil {
		t.Fatal(err)
	}

	// Third sync: prompt still gone, counter=2 → transition to running.
	if err := sync(ctx, ft, queries, homeDir, state); err != nil {
		t.Fatal(err)
	}

	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default", Path: "/home/user/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "running" {
		t.Errorf("status = %q, want %q", got, "running")
	}
}

func TestSyncGemini_WaitingStaysWhenPromptVisible(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := &fakeTmux{
		sessions:           map[string]bool{"muxac-default@home@user@project": true},
		capturePaneOutputs: map[string]string{"muxac-default@home@user@project": "Action Required\n? Shell Command  echo hello\nAllow once\nAllow for this session\nNo, suggest changes (esc)\n"},
	}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
		AgentTool: "gemini", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
		AgentSessionID: "abcdefgh-1234", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}

	state := newMonitorState()

	// Multiple syncs with prompt visible should keep status as waiting.
	for i := range 5 {
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default", Path: "/home/user/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "waiting" {
		t.Errorf("status = %q, want %q", got, "waiting")
	}
}

func TestSyncGemini_WaitingDoesNotRevertWhenPromptNeverSeen(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ft := &fakeTmux{
		sessions:           map[string]bool{"muxac-default@home@user@project": true},
		capturePaneOutputs: map[string]string{"muxac-default@home@user@project": "Some random output\n"},
	}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
		AgentTool: "gemini", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
		AgentSessionID: "abcdefgh-1234", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}

	state := newMonitorState()

	// Prompt was never seen, so should not revert even after many syncs.
	for i := range 5 {
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default", Path: "/home/user/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "waiting" {
		t.Errorf("status = %q, want %q (prompt was never seen, should not revert)", got, "waiting")
	}
}

func TestTerminalShowsGeminiWaitingPrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "shell command confirmation",
			output: "Action Required\n? Shell Command  echo hello\nAllow once\nAllow for this session\nNo, suggest changes (esc)\n",
			want:   true,
		},
		{
			name:   "edit confirmation with file scope",
			output: "Action Required\n? Edit  main.go\nAllow once\nAllow for this file in all future sessions\nNo, suggest changes (esc)\n",
			want:   true,
		},
		{
			name:   "mcp tool confirmation",
			output: "Action Required\nAllow tool for this session\nAllow all server tools for this session\nAllow tool for all future sessions\nNo, suggest changes (esc)\n",
			want:   true,
		},
		{
			name:   "allow for all future sessions",
			output: "? Shell Command  rm -rf /\nAllow once\nAllow for all future sessions\nNo, suggest changes (esc)\n",
			want:   true,
		},
		{
			name:   "allow this command for all future sessions",
			output: "? Shell Command  ls\nAllow once\nAllow this command for all future sessions\nNo, suggest changes (esc)\n",
			want:   true,
		},
		{
			name:   "action required header only",
			output: "Action Required\nLoading...\n",
			want:   true,
		},
		{
			name:   "no prompt visible",
			output: "Processing files...\nDone.\n",
			want:   false,
		},
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
		{
			name:   "non-breaking spaces normalized",
			output: "Action\u00a0Required\n",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := terminalShowsGeminiWaitingPrompt(tt.output)
			if got != tt.want {
				t.Errorf("terminalShowsGeminiWaitingPrompt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexShowsActivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "agent working - esc to interrupt visible",
			output: "• Working (3s • esc to interrupt)\n\n› Ask Codex to do anything\n",
			want:   true,
		},
		{
			name:   "exec approval prompt visible",
			output: "Some output\n  Would you like to run the following command?\n  ls -la\n  Allow once   Allow for this session   Deny\n",
			want:   true,
		},
		{
			name:   "patch approval prompt visible",
			output: "  Would you like to apply this patch?\n  Allow once   Always allow   Deny\n",
			want:   true,
		},
		{
			name:   "permissions prompt visible",
			output: "  Would you like to grant these permissions?\n",
			want:   true,
		},
		{
			name:   "network approval prompt visible",
			output: "  Do you want to approve network access to \"api.example.com\"?\n",
			want:   true,
		},
		{
			name:   "idle composer only",
			output: "› Ask Codex to do anything\n  ? for shortcuts  100% context left\n",
			want:   false,
		},
		{
			name:   "interrupted then idle",
			output: "Interrupted by user\n› Ask Codex to do anything\n",
			want:   false,
		},
		{
			name:   "empty output is treated as busy",
			output: "",
			want:   true,
		},
		{
			name:   "whitespace only is treated as busy",
			output: "   \n   \n",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := codexShowsActivity(tt.output)
			if got != tt.want {
				t.Errorf("codexShowsActivity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClaudeShowsActivity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "running with down-token readout",
			output: "● working on it\n\n✢ Running… (9m 45s · ↓ 41.0k tokens)\n────\n❯ \n  21% | 5h: 6% (3h 57m)",
			want:   true,
		},
		{
			name:   "cogitating with up-token readout",
			output: "✻ Cogitating… (3s · ↑ 4.2k tokens)\n❯ \n",
			want:   true,
		},
		{
			// Just after a prompt is submitted the spinner shows no readout yet.
			name:   "running spinner without readout",
			output: "✻ Worked for 7m 31s\n❯ update the doc\n· Running…\n────\n❯ \n  27% | 5h: 15% (0h 49m)",
			want:   true,
		},
		{
			name:   "finished turn (past tense) is not busy",
			output: "✻ Worked for 7m 31s\n────\n❯ \n────\n  27% | 5h: 15% (0h 49m) | 7d: 38% (Mon 23:00)",
			want:   false,
		},
		{
			name:   "fold indicator ellipsis is not busy",
			output: "● Read(main.go)\n  … +15 lines (ctrl+o to expand)\n────\n❯ \n  17% | 5h: 15% (0h 52m)",
			want:   false,
		},
		{
			name:   "ellipsis in shown code is not busy",
			output: "  // …\n  components.forEach((fn, name) => { /* … */ });\n────\n❯ \n  42% | 5h: 7% (3h 51m)",
			want:   false,
		},
		{
			name:   "esc to interrupt variant (case-insensitive)",
			output: "✶ Thinking… (5s · Esc to interrupt)\n❯ \n",
			want:   true,
		},
		{
			// The agent is busy, but a multi-line draft in the composer (the
			// user queuing their next message) pushes the live status line
			// above the tail window. "esc to interrupt" must still be detected
			// across the whole pane so the session is not misread as idle.
			name: "esc to interrupt above a multi-line draft is still busy",
			output: "✢ Running… (12s · esc to interrupt)\n" +
				"╭──────────────────────────────╮\n" +
				"│ > draft line 1                │\n" +
				"│ draft line 2                  │\n" +
				"│ draft line 3                  │\n" +
				"│ draft line 4                  │\n" +
				"│ draft line 5                  │\n" +
				"│ draft line 6                  │\n" +
				"╰──────────────────────────────╯\n" +
				"  21% | 5h: 6% (3h 57m)        max",
			want: true,
		},
		{
			name:   "idle with percentage status line is not busy",
			output: "╰──────────────╯\n❯ \n  21% | 5h: 6% (3h 57m) | 7d: 36% (Mon 23:00)        max",
			want:   false,
		},
		{
			name:   "idle composer only is not busy",
			output: "╭──────────────╮\n│ > do the thing │\n╰──────────────╯\n",
			want:   false,
		},
		{
			// The "↑/↓ N tokens" readout appears only on the live status line, so
			// it is trusted across the whole pane: a multi-line composer draft (the
			// user queuing their next message) scrolls the status line above the
			// tail window, but the running turn is still detected as busy. This is
			// the common case for Claude versions whose status line shows the token
			// readout but not an "esc to interrupt" hint.
			name: "token readout above a multi-line draft is still busy",
			output: "✢ Running… (5m 12s · ↓ 12.3k tokens)\n" +
				"────────────────────────\n" +
				"❯ user is typing\n" +
				"  a multi line\n" +
				"  draft message\n" +
				"  that is fairly long\n" +
				"  spanning many lines\n" +
				"  to queue up next\n" +
				"────────────────────────\n" +
				"  21% | 5h: 6% (3h 57m)        Remote Control active",
			want: true,
		},
		{
			// A token mention without the ↑/↓ readout arrow (e.g. the
			// "/clear to save N tokens" status-line hint) is not the live readout
			// and must not be mistaken for activity, even outside the tail window.
			name:   "non-readout token mention is ignored",
			output: "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\n❯ \n  21% | 5h: 6% (3h 51m)        new task? /clear to save 5.0k tokens",
			want:   false,
		},
		{
			name:   "empty output is treated as busy",
			output: "",
			want:   true,
		},
		{
			name:   "whitespace only is treated as busy",
			output: "   \n   \n",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := claudeShowsActivity(tt.output)
			if got != tt.want {
				t.Errorf("claudeShowsActivity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyncCodex(t *testing.T) {
	t.Parallel()

	t.Run("running stays running while esc to interrupt is visible", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions: map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{
				"muxac-default@home@user@project": "• Working (3s • esc to interrupt)\n",
			},
		}
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

		state := newMonitorState()
		for range 5 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (busy indicator should keep status)", got)
		}
	})

	t.Run("waiting stays waiting while permission prompt is visible", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions: map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{
				"muxac-default@home@user@project": "Would you like to run the following command?\n  ls\n  Allow once   Deny\n",
			},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		state := newMonitorState()
		for range 5 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "waiting" {
			t.Errorf("got %q, want waiting (permission prompt should keep status)", got)
		}
	})

	t.Run("running transitions to idle after two clear ticks", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions: map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{
				"muxac-default@home@user@project": "Interrupted by user\n› Ask Codex to do anything\n",
			},
		}
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

		state := newMonitorState()

		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Fatalf("after 1st sync: got %q, want running (debounce)", got)
		}

		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		got, err = queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("after 2nd sync: got %q, want idle (Ctrl+C interrupt)", got)
		}
	})

	t.Run("waiting transitions to idle after Ctrl+C dismisses permission prompt", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions: map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{
				"muxac-default@home@user@project": "› Ask Codex to do anything\n",
			},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		state := newMonitorState()
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle (Ctrl+C should transition waiting to idle)", got)
		}
	})

	t.Run("idle status is a no-op", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions: map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{
				"muxac-default@home@user@project": "› Ask Codex to do anything\n",
			},
		}
		queries := database.SetupTestDB(t)
		homeDir := t.TempDir()

		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "idle", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}

		state := newMonitorState()
		for range 5 {
			if err := sync(ctx, ft, queries, homeDir, state); err != nil {
				t.Fatal(err)
			}
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "idle" {
			t.Errorf("got %q, want idle (no-op for non-running/waiting)", got)
		}
	})

	t.Run("counter resets when activity reappears mid-debounce", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		ft := &fakeTmux{
			sessions: map[string]bool{"muxac-default@home@user@project": true},
			capturePaneOutputs: map[string]string{
				"muxac-default@home@user@project": "› Ask Codex to do anything\n",
			},
		}
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

		state := newMonitorState()

		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		ft.capturePaneOutputs["muxac-default@home@user@project"] = "• Working (1s • esc to interrupt)\n"
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		ft.capturePaneOutputs["muxac-default@home@user@project"] = "› Ask Codex to do anything\n"
		if err := sync(ctx, ft, queries, homeDir, state); err != nil {
			t.Fatal(err)
		}

		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Errorf("got %q, want running (counter should reset when activity reappeared)", got)
		}
	})
}

func TestReadLastLines(t *testing.T) {
	t.Parallel()

	t.Run("fewer than N lines returns all", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(t.TempDir(), "few.txt")
		if err := os.WriteFile(f, []byte("line1\nline2\nline3\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		lines, err := readLastLines(f, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 3 {
			t.Fatalf("got %d lines, want 3", len(lines))
		}
		if lines[0] != "line1" || lines[1] != "line2" || lines[2] != "line3" {
			t.Errorf("got %v, want [line1 line2 line3]", lines)
		}
	})

	t.Run("more than N lines returns last N", func(t *testing.T) {
		t.Parallel()
		var content strings.Builder
		for i := 1; i <= 20; i++ {
			fmt.Fprintf(&content, "line%d\n", i)
		}
		f := filepath.Join(t.TempDir(), "many.txt")
		if err := os.WriteFile(f, []byte(content.String()), 0o600); err != nil {
			t.Fatal(err)
		}

		lines, err := readLastLines(f, 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 5 {
			t.Fatalf("got %d lines, want 5", len(lines))
		}
		for i, want := range []string{"line16", "line17", "line18", "line19", "line20"} {
			if lines[i] != want {
				t.Errorf("lines[%d] = %q, want %q", i, lines[i], want)
			}
		}
	})

	t.Run("trailing newline produces no empty entry", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(t.TempDir(), "trail.txt")
		if err := os.WriteFile(f, []byte("a\nb\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		lines, err := readLastLines(f, 10)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range lines {
			if l == "" {
				t.Errorf("lines[%d] is empty", i)
			}
		}
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want 2", len(lines))
		}
	})

	t.Run("empty file returns empty slice", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(t.TempDir(), "empty.txt")
		if err := os.WriteFile(f, []byte{}, 0o600); err != nil {
			t.Fatal(err)
		}

		lines, err := readLastLines(f, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 0 {
			t.Errorf("got %d lines, want 0", len(lines))
		}
	})

	t.Run("nonexistent file returns IsNotExist error", func(t *testing.T) {
		t.Parallel()
		_, err := readLastLines(filepath.Join(t.TempDir(), "nope.txt"), 10)
		if !os.IsNotExist(err) {
			t.Errorf("expected IsNotExist, got %v", err)
		}
	})

	t.Run("large line over 70KB", func(t *testing.T) {
		t.Parallel()
		big := strings.Repeat("X", 70*1024)
		f := filepath.Join(t.TempDir(), "big.txt")
		if err := os.WriteFile(f, []byte("first\n"+big+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		lines, err := readLastLines(f, 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want 2", len(lines))
		}
		if lines[1] != big {
			t.Errorf("large line length = %d, want %d", len(lines[1]), len(big))
		}
	})

	t.Run("file without trailing newline", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(t.TempDir(), "notrail.txt")
		if err := os.WriteFile(f, []byte("a\nb\nc"), 0o600); err != nil {
			t.Fatal(err)
		}

		lines, err := readLastLines(f, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 3 {
			t.Fatalf("got %d lines, want 3", len(lines))
		}
		if lines[2] != "c" {
			t.Errorf("last line = %q, want %q", lines[2], "c")
		}
	})
}
