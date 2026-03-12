package newcmd_test

import (
	"context"
	"strings"
	"testing"

	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/newcmd"
)

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

func TestRun_NewSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, tmux, queries, "default", "/home/user/project", "/path/to/tmux.conf", "claude", t.TempDir(), []string{"MY_VAR=hello"})
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
	if ns.Command != "claude" {
		t.Errorf("command = %q, want %q", ns.Command, "claude")
	}
	if len(ns.Env) < 4 || ns.Env[0] != "MY_VAR=hello" || ns.Env[1] != "MUXAC_SESSION_NAME=default" || ns.Env[2] != "CODEX_TUI_RECORD_SESSION=1" {
		t.Errorf("env = %v, want [MY_VAR=hello MUXAC_SESSION_NAME=default CODEX_TUI_RECORD_SESSION=1 CODEX_TUI_SESSION_LOG_PATH=...]", ns.Env)
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
	if got != "stopped" {
		t.Errorf("status = %q, want %q", got, "stopped")
	}
}

func TestRun_SessionAlreadyExists(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	tmux.sessions["muxac-default@home@user@project"] = true
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, tmux, queries, "default", "/home/user/project", "", "claude", t.TempDir(), nil)
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

	err := newcmd.Run(ctx, tmux, queries, "foo", "/home/user/project", "", "claude", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(tmux.newSessions))
	}

	ns := tmux.newSessions[0]
	if len(ns.Env) < 3 || ns.Env[0] != "MUXAC_SESSION_NAME=foo" || ns.Env[1] != "CODEX_TUI_RECORD_SESSION=1" {
		t.Errorf("env = %v, want [MUXAC_SESSION_NAME=foo CODEX_TUI_RECORD_SESSION=1 CODEX_TUI_SESSION_LOG_PATH=...]", ns.Env)
	}
}

func TestRun_EnvVarsForwarded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	env := []string{"MY_VAR=hello", "OTHER_VAR=world"}
	err := newcmd.Run(ctx, tmux, queries, "default", "/home/user/project", "", "claude", t.TempDir(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.newSessions) != 1 {
		t.Fatalf("expected 1 new session, got %d", len(tmux.newSessions))
	}

	ns := tmux.newSessions[0]
	if len(ns.Env) != 5 {
		t.Fatalf("env length = %d, want 5", len(ns.Env))
	}
	if ns.Env[0] != "MY_VAR=hello" {
		t.Errorf("env[0] = %q, want %q", ns.Env[0], "MY_VAR=hello")
	}
	if ns.Env[1] != "OTHER_VAR=world" {
		t.Errorf("env[1] = %q, want %q", ns.Env[1], "OTHER_VAR=world")
	}
	if ns.Env[2] != "MUXAC_SESSION_NAME=default" {
		t.Errorf("env[2] = %q, want %q", ns.Env[2], "MUXAC_SESSION_NAME=default")
	}
	if ns.Env[3] != "CODEX_TUI_RECORD_SESSION=1" {
		t.Errorf("env[3] = %q, want %q", ns.Env[3], "CODEX_TUI_RECORD_SESSION=1")
	}
	if !strings.HasPrefix(ns.Env[4], "CODEX_TUI_SESSION_LOG_PATH=") {
		t.Errorf("env[4] = %q, want prefix CODEX_TUI_SESSION_LOG_PATH=", ns.Env[4])
	}
}

func TestRun_EmptyTmuxConf(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)

	err := newcmd.Run(ctx, tmux, queries, "default", "/home/user/project", "", "claude", t.TempDir(), nil)
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

	err := newcmd.Run(ctx, tmux, queries, "default", "/home/user/project", "/custom/tmux.conf", "claude", t.TempDir(), nil)
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
