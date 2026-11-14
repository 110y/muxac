package newcmd_test

import (
	"context"
	"strings"
	"testing"

	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/newcmd"
	"github.com/110y/muxac/internal/timestamp"
)

const testExe = "/usr/local/bin/muxac"

type fakeTmux struct {
	sessions    map[string]bool
	newSessions []newSessionCall
}

type newSessionCall struct {
	Name       string
	Env        []string
	Command    string
	SourceFile string
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

func (f *fakeTmux) NewSession(_ context.Context, name string, env []string, command string, sourceFile string) error {
	f.newSessions = append(f.newSessions, newSessionCall{
		Name:       name,
		Env:        env,
		Command:    command,
		SourceFile: sourceFile,
	})
	return nil
}

func (f *fakeTmux) ListSessionNames(_ context.Context) ([]string, error) {
	var names []string
	for name := range f.sessions {
		names = append(names, name)
	}
	return names, nil
}

func (f *fakeTmux) KillSession(_ context.Context, _ string) error {
	return nil
}

func (f *fakeTmux) NewDetachedSession(_ context.Context, _ string, _ string) error {
	return nil
}

func (f *fakeTmux) CapturePane(_ context.Context, _ string) (string, error) {
	return "", nil
}

func TestRun_NewSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, tmux, queries, testExe, "default", "/home/user/project", "/path/to/tmux.conf", "claude", []string{"MY_VAR=hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(tmux.newSessions))
	}

	ns := tmux.newSessions[0]
	if ns.Name != "muxac-default@home@user@project" {
		t.Errorf("session name = %q, want %q", ns.Name, "muxac-default@home@user@project")
	}
	wantCommand := "'/usr/local/bin/muxac' wrap --name 'default' --dir '/home/user/project' -- env -u CLAUDE_CODE_SESSION_ID -u CLAUDECODE -u CLAUDE_PROJECT_DIR claude"
	if ns.Command != wantCommand {
		t.Errorf("command = %q, want %q", ns.Command, wantCommand)
	}
	wantEnv := []string{"MY_VAR=hello", "MUXAC_SESSION_NAME=default"}
	if len(ns.Env) != len(wantEnv) {
		t.Fatalf("env length = %d, want %d (env = %v)", len(ns.Env), len(wantEnv), ns.Env)
	}
	for i, want := range wantEnv {
		if ns.Env[i] != want {
			t.Errorf("env[%d] = %q, want %q", i, ns.Env[i], want)
		}
	}
	if ns.SourceFile != "/path/to/tmux.conf" {
		t.Errorf("sourceFile = %q, want %q", ns.SourceFile, "/path/to/tmux.conf")
	}

	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default",
		Path: "/home/user/project",
	})
	if err != nil {
		t.Fatalf("failed to get session status: %v", err)
	}
	if got != "idle" {
		t.Errorf("status = %q, want %q", got, "idle")
	}
}

func TestRun_SessionAlreadyExists(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	tmux.sessions["muxac-default@home@user@project"] = true
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, tmux, queries, testExe, "default", "/home/user/project", "", "claude", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if len(tmux.newSessions) != 0 {
		t.Errorf("expected no new sessions, got %v", tmux.newSessions)
	}
}

func TestRun_InjectsMUXACSessionName(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, tmux, queries, testExe, "foo", "/home/user/project", "", "claude", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(tmux.newSessions))
	}

	ns := tmux.newSessions[0]
	if len(ns.Env) != 1 || ns.Env[0] != "MUXAC_SESSION_NAME=foo" {
		t.Errorf("env = %v, want [MUXAC_SESSION_NAME=foo]", ns.Env)
	}
}

func TestRun_EnvVarsForwarded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	env := []string{"MY_VAR=hello", "OTHER_VAR=world"}
	err := newcmd.Run(ctx, tmux, queries, testExe, "default", "/home/user/project", "", "claude", env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(tmux.newSessions))
	}

	ns := tmux.newSessions[0]
	wantEnv := []string{"MY_VAR=hello", "OTHER_VAR=world", "MUXAC_SESSION_NAME=default"}
	if len(ns.Env) != len(wantEnv) {
		t.Fatalf("env length = %d, want %d (env = %v)", len(ns.Env), len(wantEnv), ns.Env)
	}
	for i, want := range wantEnv {
		if ns.Env[i] != want {
			t.Errorf("env[%d] = %q, want %q", i, ns.Env[i], want)
		}
	}
}

func TestRun_EmptyTmuxConf(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, tmux, queries, testExe, "default", "/home/user/project", "", "claude", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(tmux.newSessions))
	}

	if tmux.newSessions[0].SourceFile != "" {
		t.Errorf("sourceFile = %q, want empty", tmux.newSessions[0].SourceFile)
	}
}

func TestRun_CustomTmuxConf(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, tmux, queries, testExe, "default", "/home/user/project", "/custom/tmux.conf", "claude", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(tmux.newSessions))
	}

	if tmux.newSessions[0].SourceFile != "/custom/tmux.conf" {
		t.Errorf("sourceFile = %q, want %q", tmux.newSessions[0].SourceFile, "/custom/tmux.conf")
	}
}

func TestRun_StripsInheritedAgentSessionEnv(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, tmux, queries, testExe, "default", "/home/user/project", "", "claude", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(tmux.newSessions))
	}

	want := " -- env -u CLAUDE_CODE_SESSION_ID -u CLAUDECODE -u CLAUDE_PROJECT_DIR claude"
	if got := tmux.newSessions[0].Command; !strings.HasSuffix(got, want) {
		t.Errorf("command = %q, want suffix %q", got, want)
	}
}

func TestRun_WrapsCommandForProgramStatus(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	// The name, directory and executable path are quoted as single shell words;
	// the command is left for the shell to parse as before.
	err := newcmd.Run(ctx, tmux, queries, "/opt/my tools/muxac", "it's", "/home/user/my project", "", "FOO=bar claude --model 'opus'", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(tmux.newSessions))
	}

	want := `'/opt/my tools/muxac' wrap --name 'it'\''s' --dir '/home/user/my project' -- env -u CLAUDE_CODE_SESSION_ID -u CLAUDECODE -u CLAUDE_PROJECT_DIR FOO=bar claude --model 'opus'`
	if got := tmux.newSessions[0].Command; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func TestRun_ResetsProgramStatus(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	// A stale row left by an earlier session whose agent reported program
	// status must not make the new session ignore hook events.
	if err := queries.EnsureSession(ctx, sqlc.EnsureSessionParams{
		Name:      "default",
		Path:      "/home/user/project",
		Status:    "running",
		UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatalf("failed to seed session: %v", err)
	}
	if err := queries.UpdateSessionProgramStatus(ctx, sqlc.UpdateSessionProgramStatusParams{
		Status:    "running",
		UpdatedAt: timestamp.Now(),
		Name:      "default",
		Path:      "/home/user/project",
	}); err != nil {
		t.Fatalf("failed to seed session: %v", err)
	}

	if err := newcmd.Run(ctx, tmux, queries, testExe, "default", "/home/user/project", "", "claude", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	programStatus, err := queries.GetSessionProgramStatus(ctx, sqlc.GetSessionProgramStatusParams{
		Name: "default",
		Path: "/home/user/project",
	})
	if err != nil {
		t.Fatalf("failed to get program status: %v", err)
	}
	if programStatus != 0 {
		t.Errorf("program_status = %d, want 0", programStatus)
	}
	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default",
		Path: "/home/user/project",
	})
	if err != nil {
		t.Fatalf("failed to get session status: %v", err)
	}
	if got != "idle" {
		t.Errorf("status = %q, want %q", got, "idle")
	}
}
