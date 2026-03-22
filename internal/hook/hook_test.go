package hook_test

import (
	"strings"
	"testing"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/hook"
	"github.com/110y/muxac/internal/timestamp"
)

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
			tool:        agent.Claude,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "running",
		},
		{
			name:        "PermissionRequest writes waiting",
			input:       `{"hook_event_name": "PermissionRequest"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Claude,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "waiting",
		},
		{
			name:        "Stop writes idle",
			input:       `{"hook_event_name": "Stop"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Claude,
			wantName:    "default",
			wantPath:    "/home/user/project",
			wantStatus:  "idle",
		},
		{
			name:        "custom session name",
			input:       `{"hook_event_name": "Stop"}`,
			sessionName: "foo",
			projectDir:  "/home/user/project",
			tool:        agent.Claude,
			wantName:    "foo",
			wantPath:    "/home/user/project",
			wantStatus:  "idle",
		},
		{
			name:        "unknown event is no-op",
			input:       `{"hook_event_name": "SomeOtherEvent"}`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Claude,
			wantNoop:    true,
		},
		{
			name:        "empty session name is no-op",
			input:       `{"hook_event_name": "Stop"}`,
			sessionName: "",
			projectDir:  "/home/user/project",
			tool:        agent.Claude,
			wantNoop:    true,
		},
		{
			name:        "malformed JSON returns error",
			input:       `{not json`,
			sessionName: "default",
			projectDir:  "/home/user/project",
			tool:        agent.Claude,
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			queries := database.SetupTestDB(t)
			r := strings.NewReader(tt.input)

			err := hook.Run(ctx, r, queries, tt.sessionName, tt.projectDir, tt.tool)
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
			name:          "waiting + PreToolUse becomes running",
			initialStatus: "waiting",
			input:         `{"hook_event_name": "PreToolUse"}`,
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
			if err := hook.Run(ctx, r, queries, "default", "/project", agent.Claude); err != nil {
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
			if err := hook.Run(ctx, r, queries, "default", "/project", agent.Gemini); err != nil {
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
	if err := hook.Run(ctx, r, queries, "default", "/project", agent.Gemini); err != nil {
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

func TestRunSessionStart_SavesSessionID(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	// Pre-create session
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/project", Status: "running", UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// SessionStart with session_id
	r := strings.NewReader(`{"hook_event_name": "SessionStart", "session_id": "abc-123"}`)
	if err := hook.Run(ctx, r, queries, "default", "/project", agent.Claude); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify session ID was saved
	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].AgentSessionID != "abc-123" {
		t.Errorf("agent_session_id = %q, want %q", sessions[0].AgentSessionID, "abc-123")
	}
}

func TestRunPermissionRequest_SetsWaitingSince(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	// Pre-create session as running
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/project", Status: "running", UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	before := timestamp.Now()
	r := strings.NewReader(`{"hook_event_name": "PermissionRequest"}`)
	if err := hook.Run(ctx, r, queries, "default", "/project", agent.Claude); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].WaitingSince == "" {
		t.Fatal("waiting_since should be set on PermissionRequest")
	}
	if sessions[0].WaitingSince < before {
		t.Errorf("waiting_since = %q, expected >= %q", sessions[0].WaitingSince, before)
	}
}

func TestRunUserPromptSubmit_ClearsWaitingSince(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	// Pre-create session as waiting with waiting_since set
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/project", Status: "waiting", UpdatedAt: timestamp.Now(), WaitingSince: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	r := strings.NewReader(`{"hook_event_name": "UserPromptSubmit"}`)
	if err := hook.Run(ctx, r, queries, "default", "/project", agent.Claude); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].WaitingSince != "" {
		t.Errorf("waiting_since = %q, want empty after UserPromptSubmit", sessions[0].WaitingSince)
	}
}

func TestRunSessionStart_SavesAgentTool(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	// Pre-create session
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: "default", Path: "/project", Status: "running", UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// SessionStart with Claude tool
	r := strings.NewReader(`{"hook_event_name": "SessionStart", "session_id": "abc-123"}`)
	if err := hook.Run(ctx, r, queries, "default", "/project", agent.Claude); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify agent_tool was saved
	sessions, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].AgentTool != "claude" {
		t.Errorf("agent_tool = %q, want %q", sessions[0].AgentTool, "claude")
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
	if err := hook.Run(ctx, r, queries, "default", "/project", agent.Claude); err != nil {
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
