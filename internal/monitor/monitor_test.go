package monitor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/pathkey"
	"github.com/110y/muxac/internal/timestamp"
)

// fakeScreen plays the screen of the session the tests sync.
type fakeScreen struct {
	output string
}

func (f *fakeScreen) screen() string {
	return f.output
}

// fakeSessions plays the servers of the running sessions.
type fakeSessions struct {
	names []string
	err   error
}

func (f *fakeSessions) ListSessionNames(context.Context) ([]string, error) {
	return f.names, f.err
}

var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestSync(t *testing.T) {
	t.Parallel()

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

		// Simulate: monitor reads session as "waiting", then hook changes to "idle"
		// before monitor's CAS write
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name: "default", Path: "/home/user/project", Status: "idle", UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		// CAS should be a no-op because status is now "idle", not "waiting"
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
		if got != "idle" {
			t.Errorf("got %q, want idle (CAS should be no-op)", got)
		}
	})
}

func TestSyncGemini_NoFileSyncNeeded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := &fakeScreen{}
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

	if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, newMonitorState()); err != nil {
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
	fm := &fakeScreen{}
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

	if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, newMonitorState()); err != nil {
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
	fm := &fakeScreen{}
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

	if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, newMonitorState()); err != nil {
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
	fm := &fakeScreen{}
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

	if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, newMonitorState()); err != nil {
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

func TestSyncGemini_WaitingToRunningViaScreenCaptureWhenPromptDismissed(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := &fakeScreen{
		output: "Action Required\n? Shell Command  echo hello\nAllow once\nAllow for this session\nNo, suggest changes (esc)\n",
	}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
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
	if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
		t.Fatal(err)
	}

	// Prompt disappears (user approved).
	fm.output = "Processing files...\n"

	// Second sync: prompt gone, counter=1 (debounce).
	if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
		t.Fatal(err)
	}

	// Third sync: prompt still gone, counter=2 → transition to running.
	if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
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
	fm := &fakeScreen{
		output: "Action Required\n? Shell Command  echo hello\nAllow once\nAllow for this session\nNo, suggest changes (esc)\n",
	}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
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
		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
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
	fm := &fakeScreen{
		output: "Some random output\n",
	}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "waiting", UpdatedAt: timestamp.Now(),
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
		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
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

func TestSyncCodex(t *testing.T) {
	t.Parallel()

	t.Run("running stays running while esc to interrupt is visible", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		fm := &fakeScreen{
			output: "• Working (3s • esc to interrupt)\n",
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
			if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
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
		fm := &fakeScreen{
			output: "Would you like to run the following command?\n  ls\n  Allow once   Deny\n",
		}
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

		state := newMonitorState()
		for range 5 {
			if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
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
		fm := &fakeScreen{
			output: "Interrupted by user\n› Ask Codex to do anything\n",
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

		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
			t.Fatal(err)
		}
		got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{Name: "default", Path: "/home/user/project"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "running" {
			t.Fatalf("after 1st sync: got %q, want running (debounce)", got)
		}

		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
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
		fm := &fakeScreen{
			output: "› Ask Codex to do anything\n",
		}
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

		state := newMonitorState()
		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
			t.Fatal(err)
		}
		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
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
		fm := &fakeScreen{
			output: "› Ask Codex to do anything\n",
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
			if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
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
		fm := &fakeScreen{
			output: "› Ask Codex to do anything\n",
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

		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
			t.Fatal(err)
		}

		fm.output = "• Working (1s • esc to interrupt)\n"
		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
			t.Fatal(err)
		}

		fm.output = "› Ask Codex to do anything\n"
		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
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

func TestSync_ProgramStatusSessionIsLeftAlone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := &fakeScreen{
		// A screen the Codex heuristic reads as an interrupted turn, which
		// moves a running session to idle after two ticks.
		output: "Interrupted by user\n› Ask Codex to do anything\n",
	}
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()

	if err := queries.EnsureSession(ctx, sqlc.EnsureSessionParams{
		Name: "default", Path: "/home/user/project", Status: "idle", UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
		AgentTool: "codex", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}
	// The agent reported running through OSC 7501.
	if err := queries.UpdateSessionProgramStatus(ctx, sqlc.UpdateSessionProgramStatusParams{
		Status: "running", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
	}); err != nil {
		t.Fatal(err)
	}

	state := newMonitorState()
	for range 3 {
		if err := syncNamedSession(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, state); err != nil {
			t.Fatal(err)
		}
	}

	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default", Path: "/home/user/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "running" {
		t.Errorf("status = %q, want running (left to the agent's OSC 7501 reports)", got)
	}
	if n := state.captureClearCount["default:/home/user/project"]; n != 0 {
		t.Errorf("debounce counter = %d, want none kept for a program status session", n)
	}
}

func TestSync_ClaudeSessionIsLeftAlone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := &fakeScreen{
		output: "Do you want to proceed?\n❯ 1. Yes\n  2. No\n\nEsc to cancel",
	}
	queries := database.SetupTestDB(t)

	// A row left by an older muxac whose hooks recorded Claude Code: nothing
	// is inferred for it, whatever the screen shows.
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
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
		if err := syncNamedSession(ctx, queries, t.TempDir(), "default", "/home/user/project", fm.screen, state); err != nil {
			t.Fatal(err)
		}
	}

	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default", Path: "/home/user/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "running" {
		t.Errorf("status = %q, want running (Claude Code sessions are tracked by muxac wrap only)", got)
	}
}

func TestSyncNamedSession_MakesMissingRowAgain(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	// The session runs, but its row is gone, as when Prune ran while the
	// session was starting.
	if err := syncNamedSession(ctx, queries, t.TempDir(), "default", "/home/user/project", (&fakeScreen{}).screen, newMonitorState()); err != nil {
		t.Fatal(err)
	}

	got, err := queries.GetSession(ctx, sqlc.GetSessionParams{Name: "default", Path: "/home/user/project"})
	if err != nil {
		t.Fatalf("the row was not made again: %v", err)
	}
	if got.Status != "idle" || got.ProgramStatus != 0 {
		t.Errorf("the row was made again as %+v, want an idle one", got)
	}
}

// waitFor waits for cond to hold, failing the test if it does not in time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queries := database.SetupTestDB(t)
	homeDir := t.TempDir()
	params := sqlc.GetSessionParams{Name: "default", Path: "/home/user/project"}

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
	// Codex was interrupted: its screen no longer shows it busy.
	fm := &fakeScreen{output: "Interrupted by user\n› Ask Codex to do anything\n"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, queries, homeDir, "default", "/home/user/project", fm.screen, discardLogger)
	}()

	waitFor(t, "the status of the session to be brought up to date", func() bool {
		got, err := queries.GetSession(ctx, params)
		return err == nil && got.Status == "idle"
	})

	if err := queries.DeleteSession(ctx, sqlc.DeleteSessionParams{Name: "default", Path: "/home/user/project"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the row of the running session to be made again", func() bool {
		_, err := queries.GetSession(ctx, params)
		return err == nil
	})

	// The session ends.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not return once the session ended")
	}
	if _, err := queries.GetSession(t.Context(), params); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetSession once the session ended: %v, want the row deleted", err)
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()

	// setup makes the rows of the sessions named so in /home/user/project:
	// "running" runs, "ended" and "reporting", whose agent reported its
	// status through OSC 7501, no longer do.
	setup := func(t *testing.T) *sqlc.Queries {
		t.Helper()

		ctx := t.Context()
		queries := database.SetupTestDB(t)
		for _, name := range []string{"running", "ended", "reporting"} {
			if err := queries.EnsureSession(ctx, sqlc.EnsureSessionParams{
				Name: name, Path: "/home/user/project", Status: "idle", UpdatedAt: timestamp.Now(),
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := queries.UpdateSessionProgramStatus(ctx, sqlc.UpdateSessionProgramStatusParams{
			Status: "running", UpdatedAt: timestamp.Now(), Name: "reporting", Path: "/home/user/project",
		}); err != nil {
			t.Fatal(err)
		}
		return queries
	}
	names := func(t *testing.T, queries *sqlc.Queries) []string {
		t.Helper()

		rows, err := queries.ListSessions(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, row := range rows {
			names = append(names, row.Name)
		}
		slices.Sort(names)
		return names
	}

	t.Run("rows of sessions that no longer run are deleted", func(t *testing.T) {
		t.Parallel()

		queries := setup(t)
		sessions := &fakeSessions{names: []string{
			pathkey.SessionName("running", "/home/user/project"),
			pathkey.SessionName("ended", "/home/user/other"),
		}}
		if err := Prune(t.Context(), sessions, queries); err != nil {
			t.Fatal(err)
		}
		if got, want := names(t, queries), []string{"running"}; !slices.Equal(got, want) {
			t.Errorf("rows left = %q, want %q", got, want)
		}
	})

	t.Run("nothing is deleted if a server does not answer", func(t *testing.T) {
		t.Parallel()

		queries := setup(t)
		sessions := &fakeSessions{err: errors.New("ask the session: i/o timeout")}
		if err := Prune(t.Context(), sessions, queries); err == nil {
			t.Error("Prune succeeded although a server did not answer")
		}
		if got, want := names(t, queries), []string{"ended", "reporting", "running"}; !slices.Equal(got, want) {
			t.Errorf("rows left = %q, want %q", got, want)
		}
	})
}
