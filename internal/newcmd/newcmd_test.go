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

type fakeMux struct {
	sessions    map[string]bool
	newSessions []newSessionCall
}

type newSessionCall struct {
	Name    string
	Dir     string
	Env     []string
	Command string
}

func newFakeMux() *fakeMux {
	return &fakeMux{
		sessions: make(map[string]bool),
	}
}

func (f *fakeMux) HasSession(_ context.Context, sessionName string) bool {
	return f.sessions[sessionName]
}

func (f *fakeMux) AttachSession(_ context.Context, _ string) error {
	return nil
}

func (f *fakeMux) NewSession(_ context.Context, name, dir string, env []string, command string) error {
	f.newSessions = append(f.newSessions, newSessionCall{
		Name:    name,
		Dir:     dir,
		Env:     env,
		Command: command,
	})
	return nil
}

func (f *fakeMux) ListSessionNames(_ context.Context) ([]string, error) {
	var names []string
	for name := range f.sessions {
		names = append(names, name)
	}
	return names, nil
}

func (f *fakeMux) KillSession(_ context.Context, _ string) error {
	return nil
}

func TestRun_NewSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, fm, queries, testExe, "default", "/home/user/project", "claude", []string{"MY_VAR=hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(fm.newSessions))
	}

	ns := fm.newSessions[0]
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
	if ns.Dir != "/home/user/project" {
		t.Errorf("dir = %q, want %q", ns.Dir, "/home/user/project")
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
	fm := newFakeMux()
	fm.sessions["muxac-default@home@user@project"] = true
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, fm, queries, testExe, "default", "/home/user/project", "claude", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if len(fm.newSessions) != 0 {
		t.Errorf("expected no new sessions, got %v", fm.newSessions)
	}
}

func TestRun_InjectsMUXACSessionName(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, fm, queries, testExe, "foo", "/home/user/project", "claude", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(fm.newSessions))
	}

	ns := fm.newSessions[0]
	if len(ns.Env) != 1 || ns.Env[0] != "MUXAC_SESSION_NAME=foo" {
		t.Errorf("env = %v, want [MUXAC_SESSION_NAME=foo]", ns.Env)
	}
}

func TestRun_EnvVarsForwarded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()
	queries := database.SetupTestDB(t)

	env := []string{"MY_VAR=hello", "OTHER_VAR=world"}
	err := newcmd.Run(ctx, fm, queries, testExe, "default", "/home/user/project", "claude", env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(fm.newSessions))
	}

	ns := fm.newSessions[0]
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

func TestRun_RunsCommandInWorkDir(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, fm, queries, testExe, "default", "/home/user/other project", "claude", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(fm.newSessions))
	}

	if got := fm.newSessions[0].Dir; got != "/home/user/other project" {
		t.Errorf("dir = %q, want %q", got, "/home/user/other project")
	}
}

func TestRun_StripsInheritedAgentSessionEnv(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, fm, queries, testExe, "default", "/home/user/project", "claude", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(fm.newSessions))
	}

	want := " -- env -u CLAUDE_CODE_SESSION_ID -u CLAUDECODE -u CLAUDE_PROJECT_DIR claude"
	if got := fm.newSessions[0].Command; !strings.HasSuffix(got, want) {
		t.Errorf("command = %q, want suffix %q", got, want)
	}
}

func TestRun_WrapsCommandForProgramStatus(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()
	queries := database.SetupTestDB(t)

	// The name, directory and executable path are quoted as single shell words;
	// the command is left for the shell to parse as before.
	err := newcmd.Run(ctx, fm, queries, "/opt/my tools/muxac", "it's", "/home/user/my project", "FOO=bar claude --model 'opus'", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(fm.newSessions))
	}

	want := `'/opt/my tools/muxac' wrap --name 'it'\''s' --dir '/home/user/my project' -- env -u CLAUDE_CODE_SESSION_ID -u CLAUDECODE -u CLAUDE_PROJECT_DIR FOO=bar claude --model 'opus'`
	if got := fm.newSessions[0].Command; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func TestRun_ResetsProgramStatus(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()
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

	if err := newcmd.Run(ctx, fm, queries, testExe, "default", "/home/user/project", "claude", nil); err != nil {
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
