package hook_test

import (
	"context"
	"strings"
	"testing"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/hook"
	"github.com/110y/muxac/internal/timestamp"
	"github.com/110y/muxac/internal/tmux"
)

// fakeTmux is a tmux.Runner whose HasSession result is configurable.
type fakeTmux struct {
	hasSession bool
}

var _ tmux.Runner = (*fakeTmux)(nil)

func (f *fakeTmux) HasSession(_ context.Context, _ string) bool     { return f.hasSession }
func (f *fakeTmux) AttachSession(_ context.Context, _ string) error { return nil }
func (f *fakeTmux) NewSession(_ context.Context, _ string, _ []string, _, _ string) error {
	return nil
}
func (f *fakeTmux) ListSessionNames(_ context.Context) ([]string, error)    { return nil, nil }
func (f *fakeTmux) KillSession(_ context.Context, _ string) error           { return nil }
func (f *fakeTmux) NewDetachedSession(_ context.Context, _, _ string) error { return nil }
func (f *fakeTmux) CapturePane(_ context.Context, _ string) (string, error) { return "", nil }

// aliveTmux returns a fakeTmux that reports every session as alive, matching the
// normal case where a hook fires from within its own muxac-managed tmux session.
func aliveTmux() *fakeTmux { return &fakeTmux{hasSession: true} }

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		sessionName string
		projectDir  string
		tool        agent.Tool
		wantName    string
		wantPath    string
		wantStatus  string
		wantErr     bool
		wantNoop    bool
	}{
		{
			name:        "UserPromptSubmit writes running",
			input:       `{"hook_event_name": "UserPromptSubmit"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "running",
		},
		{
			name:        "PermissionRequest writes waiting",
			input:       `{"hook_event_name": "PermissionRequest"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "waiting",
		},
		{
			name:        "PostToolUse writes running",
			input:       `{"hook_event_name": "PostToolUse"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "running",
		},
		{
			name:        "Stop writes idle",
			input:       `{"hook_event_name": "Stop"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "idle",
		},
		{
			name:        "custom session name",
			input:       `{"hook_event_name": "Stop"}`,
			sessionName: "foo",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantName:    "foo",
			wantPath:    "/home/user/project",
			wantStatus:  "idle",
		},
		{
			name:        "unknown event is no-op",
			input:       `{"hook_event_name": "SomeOtherEvent"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantNoop:    true,
		},
		{
			name:        "empty session name is no-op",
			input:       `{"hook_event_name": "Stop"}`,
			sessionName: "",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantNoop:    true,
		},
		{
			name:        "malformed JSON returns error",
			input:       `{not json`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantErr:     true,
		},
		{
			name:        "unknown tool processes events",
			input:       `{"hook_event_name": "UserPromptSubmit"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Unknown,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "running",
		},
		{
			name:        "Gemini BeforeAgent writes running",
			input:       `{"hook_event_name": "BeforeAgent"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Gemini,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "running",
		},
		{
			name:        "Gemini BeforeTool writes running",
			input:       `{"hook_event_name": "BeforeTool"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Gemini,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "running",
		},
		{
			name:        "Gemini AfterAgent writes idle",
			input:       `{"hook_event_name": "AfterAgent"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Gemini,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "idle",
		},
		{
			name:        "Gemini Notification ToolPermission writes waiting",
			input:       `{"hook_event_name": "Notification", "notification_type": "ToolPermission"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Gemini,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "waiting",
		},
		{
			name:        "Gemini SessionStart writes idle",
			input:       `{"hook_event_name": "SessionStart"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Gemini,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "idle",
		},
		{
			name:        "Gemini SessionEnd writes idle",
			input:       `{"hook_event_name": "SessionEnd"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Gemini,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "idle",
		},
		{
			name:        "Gemini Notification non-ToolPermission is no-op",
			input:       `{"hook_event_name": "Notification", "notification_type": "SomethingElse"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Gemini,
			wantNoop:    true,
		},
		{
			name:        "Codex UserPromptSubmit writes running",
			input:       `{"hook_event_name": "UserPromptSubmit", "cwd": "/home/user/project"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "running",
		},
		{
			name:        "Codex PermissionRequest writes waiting",
			input:       `{"hook_event_name": "PermissionRequest", "cwd": "/home/user/project"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "waiting",
		},
		{
			name:        "Codex Stop writes idle",
			input:       `{"hook_event_name": "Stop", "cwd": "/home/user/project"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Codex,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "idle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			queries := database.SetupTestDB(t)
			r := strings.NewReader(tt.input)

			err := hook.Run(ctx, r, aliveTmux(), queries, tt.sessionName, tt.projectDir, tt.tool)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantNoop {
				rows, err := queries.ListSessions(ctx)
				if err != nil {
					t.Fatalf("unexpected error listing sessions: %v", err)
				}
				if len(rows) != 0 {
					t.Errorf("expected no sessions, got %d", len(rows))
				}
				return
			}

			got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
				Name: tt.wantName,
				Path: tt.wantPath,
			})
			if err != nil {
				t.Fatalf("failed to get session status: %v", err)
			}
			if got != tt.wantStatus {
				t.Errorf("status = %q, want %q", got, tt.wantStatus)
			}
		})
	}
}

func TestRunNoMuxacTmuxSessionIsNoop(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	// A nested agent launched in a subdirectory inherits MUXAC_SESSION_NAME but has
	// no muxac-managed tmux session for its cwd. Its hook events must not create a
	// phantom session row.
	r := strings.NewReader(`{"hook_event_name": "UserPromptSubmit"}`)
	if err := hook.Run(ctx, r, &fakeTmux{hasSession: false}, queries, "claude", "/home/user/project/sub/dir", agent.Codex); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rows, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatalf("unexpected error listing sessions: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no sessions when no muxac tmux session exists, got %d", len(rows))
	}
}

func TestRunWithCurrentState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		initialStatus string
		input         string
		wantStatus    string
	}{
		{
			name:          "waiting + UserPromptSubmit becomes running",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "UserPromptSubmit"}`,
			wantStatus:    "running",
		},
		{
			name:          "waiting + PreToolUse stays waiting",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "PreToolUse"}`,
			wantStatus:    "waiting",
		},
		{
			name:          "waiting + PostToolUse becomes running",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "PostToolUse"}`,
			wantStatus:    "running",
		},
		{
			name:          "waiting + Stop stays waiting",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "Stop"}`,
			wantStatus:    "waiting",
		},
		{
			name:          "waiting + PermissionRequest stays waiting",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "PermissionRequest"}`,
			wantStatus:    "waiting",
		},
		{
			name:          "waiting + SessionEnd becomes idle",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "SessionEnd"}`,
			wantStatus:    "idle",
		},
		{
			name:          "waiting + SessionStart becomes idle",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "SessionStart"}`,
			wantStatus:    "idle",
		},
		{
			name:          "idle + UserPromptSubmit becomes running",
			initialStatus: "idle",
			input:         `{"hook_event_name": "UserPromptSubmit"}`,
			wantStatus:    "running",
		},
		{
			name:          "idle + PreToolUse becomes running",
			initialStatus: "idle",
			input:         `{"hook_event_name": "PreToolUse"}`,
			wantStatus:    "running",
		},
		{
			name:          "idle + PermissionRequest becomes waiting",
			initialStatus: "idle",
			input:         `{"hook_event_name": "PermissionRequest"}`,
			wantStatus:    "waiting",
		},
		{
			name:          "idle + Stop stays idle",
			initialStatus: "idle",
			input:         `{"hook_event_name": "Stop"}`,
			wantStatus:    "idle",
		},
		{
			name:          "running + PermissionRequest becomes waiting",
			initialStatus: "running",
			input:         `{"hook_event_name": "PermissionRequest"}`,
			wantStatus:    "waiting",
		},
		{
			name:          "running + Stop becomes idle",
			initialStatus: "running",
			input:         `{"hook_event_name": "Stop"}`,
			wantStatus:    "idle",
		},
		{
			name:          "running + UserPromptSubmit stays running",
			initialStatus: "running",
			input:         `{"hook_event_name": "UserPromptSubmit"}`,
			wantStatus:    "running",
		},
		{
			name:          "running + PostToolUse stays running",
			initialStatus: "running",
			input:         `{"hook_event_name": "PostToolUse"}`,
			wantStatus:    "running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			queries := database.SetupTestDB(t)

			if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
				Name: "default", Path: "/project", Status: tt.initialStatus, UpdatedAt: timestamp.Now(),
			}); err != nil {
				t.Fatal(err)
			}

			r := strings.NewReader(tt.input)
			if err := hook.Run(ctx, r, aliveTmux(), queries, "default", "/project", agent.Codex); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
				Name: "default", Path: "/project",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantStatus {
				t.Errorf("status = %q, want %q", got, tt.wantStatus)
			}
		})
	}
}

func TestRunWithCurrentState_Gemini(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		initialStatus string
		input         string
		wantStatus    string
	}{
		{
			name:          "waiting + BeforeTool becomes running",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "BeforeTool"}`,
			wantStatus:    "running",
		},
		{
			name:          "waiting + AfterAgent becomes idle",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "AfterAgent"}`,
			wantStatus:    "idle",
		},
		{
			name:          "idle + BeforeAgent becomes running",
			initialStatus: "idle",
			input:         `{"hook_event_name": "BeforeAgent"}`,
			wantStatus:    "running",
		},
		{
			name:          "running + Notification ToolPermission becomes waiting",
			initialStatus: "running",
			input:         `{"hook_event_name": "Notification", "notification_type": "ToolPermission"}`,
			wantStatus:    "waiting",
		},
		{
			name:          "running + AfterAgent becomes idle",
			initialStatus: "running",
			input:         `{"hook_event_name": "AfterAgent"}`,
			wantStatus:    "idle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			queries := database.SetupTestDB(t)

			if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
				Name: "default", Path: "/project", Status: tt.initialStatus, UpdatedAt: timestamp.Now(),
			}); err != nil {
				t.Fatal(err)
			}

			r := strings.NewReader(tt.input)
			if err := hook.Run(ctx, r, aliveTmux(), queries, "default", "/project", agent.Gemini); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
				Name: "default", Path: "/project",
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantStatus {
				t.Errorf("status = %q, want %q", got, tt.wantStatus)
			}
		})
	}
}

func TestRunSessionStart_SavesAgentTool_Codex(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/project", Status: "running", UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	r := strings.NewReader(`{"hook_event_name": "SessionStart", "session_id": "codex-789", "cwd": "/project"}`)
	if err := hook.Run(ctx, r, aliveTmux(), queries, "default", "/project", agent.Codex); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].AgentTool != "codex" {
		t.Errorf("agent_tool = %q, want %q", sessions[0].AgentTool, "codex")
	}
	if sessions[0].AgentSessionID != "codex-789" {
		t.Errorf("agent_session_id = %q, want %q", sessions[0].AgentSessionID, "codex-789")
	}
}

func TestRunSessionStart_SavesAgentTool_Gemini(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/project", Status: "running", UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	r := strings.NewReader(`{"hook_event_name": "SessionStart", "session_id": "gem-456"}`)
	if err := hook.Run(ctx, r, aliveTmux(), queries, "default", "/project", agent.Gemini); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].AgentTool != "gemini" {
		t.Errorf("agent_tool = %q, want %q", sessions[0].AgentTool, "gemini")
	}
}

func TestRunSessionStart_NoSessionID(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	// Pre-create session with existing session ID
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/project", Status: "running", UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
		AgentSessionID: "existing-id", UpdatedAt: timestamp.Now(), Name: "default", Path: "/project",
	}); err != nil {
		t.Fatal(err)
	}

	// SessionStart without session_id should not overwrite
	r := strings.NewReader(`{"hook_event_name": "SessionStart"}`)
	if err := hook.Run(ctx, r, aliveTmux(), queries, "default", "/project", agent.Codex); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].AgentSessionID != "existing-id" {
		t.Errorf("agent_session_id = %q, want %q", sessions[0].AgentSessionID, "existing-id")
	}
}

func TestRunIgnoresProgramStatusSession(t *testing.T) {
	t.Parallel()

	events := []string{
		`{"hook_event_name": "UserPromptSubmit"}`,
		`{"hook_event_name": "PreToolUse"}`,
		`{"hook_event_name": "PermissionRequest"}`,
		`{"hook_event_name": "Stop"}`,
		`{"hook_event_name": "SessionStart", "session_id": "sess-1"}`,
		`{"hook_event_name": "SessionEnd"}`,
	}

	for _, input := range events {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			queries := database.SetupTestDB(t)

			// The agent reported running through OSC 7501, so `muxac wrap` owns
			// the session status even though the agent also fires hooks.
			if err := queries.EnsureSession(ctx, sqlc.EnsureSessionParams{
				Name: "default", Path: "/home/user/project", Status: "idle", UpdatedAt: timestamp.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			if err := queries.UpdateSessionProgramStatus(ctx, sqlc.UpdateSessionProgramStatusParams{
				Status: "running", UpdatedAt: timestamp.Now(), Name: "default", Path: "/home/user/project",
			}); err != nil {
				t.Fatal(err)
			}

			if err := hook.Run(ctx, strings.NewReader(input), aliveTmux(), queries, "default", "/home/user/project", agent.Codex); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			rows, err := queries.ListSessions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("sessions = %d, want 1", len(rows))
			}
			if rows[0].Status != "running" {
				t.Errorf("status = %q, want running (hook events must not override OSC 7501 reports)", rows[0].Status)
			}
			if rows[0].AgentTool != "" || rows[0].AgentSessionID != "" {
				t.Errorf("agent tool/session id = %q/%q, want them untouched", rows[0].AgentTool, rows[0].AgentSessionID)
			}
		})
	}
}

func TestRunIgnoresClaudeCode(t *testing.T) {
	t.Parallel()

	events := []string{
		`{"hook_event_name": "UserPromptSubmit"}`,
		`{"hook_event_name": "PreToolUse"}`,
		`{"hook_event_name": "PostToolUse"}`,
		`{"hook_event_name": "PermissionRequest"}`,
		`{"hook_event_name": "Notification", "notification_type": "permission_prompt"}`,
		`{"hook_event_name": "Stop"}`,
		`{"hook_event_name": "SessionStart", "session_id": "sess-1"}`,
		`{"hook_event_name": "SessionEnd"}`,
		`{not json`,
	}

	for _, input := range events {
		t.Run(input, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()

			// Claude Code's status comes from OSC 7501 only, so its hooks
			// change nothing, even before its first report arrives...
			queries := database.SetupTestDB(t)
			if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
				Name: "default", Path: "/home/user/project", Status: "running", UpdatedAt: timestamp.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			if err := hook.Run(ctx, strings.NewReader(input), aliveTmux(), queries, "default", "/home/user/project", agent.Claude); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			rows, err := queries.ListSessions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Status != "running" || rows[0].AgentTool != "" || rows[0].AgentSessionID != "" {
				t.Errorf("session = %+v, want it untouched", rows)
			}

			// ...and create no session either.
			empty := database.SetupTestDB(t)
			if err := hook.Run(ctx, strings.NewReader(input), aliveTmux(), empty, "default", "/home/user/project", agent.Claude); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			rows, err = empty.ListSessions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Errorf("sessions = %d, want none", len(rows))
			}
		})
	}
}
