package remove_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/remove"
	"github.com/110y/muxac/internal/timestamp"
)

type fakeTmux struct {
	sessions  map[string]bool
	killed    []string
	killError error
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
	f.killed = append(f.killed, sessionName)
	if f.killError != nil {
		return f.killError
	}
	delete(f.sessions, sessionName)
	return nil
}

func (f *fakeTmux) NewDetachedSession(_ context.Context, _ string, _ string) error {
	return nil
}

func (f *fakeTmux) CapturePane(_ context.Context, _ string) (string, error) {
	return "", nil
}

func seedSession(t *testing.T, queries *sqlc.Queries, name, path string) {
	t.Helper()

	if err := queries.UpsertSessionStatus(t.Context(), sqlc.UpsertSessionStatusParams{
		Name:      name,
		Path:      path,
		Status:    "idle",
		UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatalf("failed to seed session: %v", err)
	}
}

func TestRun_ExistingSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	tmux.sessions["muxac-default@home@user@project"] = true
	queries := database.SetupTestDB(t)
	seedSession(t, queries, "default", "/home/user/project")

	err := remove.Run(ctx, tmux, queries, "default", "/home/user/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.killed) != 1 || tmux.killed[0] != "muxac-default@home@user@project" {
		t.Errorf("expected kill for muxac-default@home@user@project, got %v", tmux.killed)
	}

	_, err = queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default",
		Path: "/home/user/project",
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows after delete, got %v", err)
	}
}

func TestRun_NonExistentSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	queries := database.SetupTestDB(t)
	seedSession(t, queries, "default", "/home/user/project")

	err := remove.Run(ctx, tmux, queries, "default", "/home/user/project")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want to contain %q", err, "does not exist")
	}

	if len(tmux.killed) != 0 {
		t.Errorf("expected no kill calls, got %v", tmux.killed)
	}

	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default",
		Path: "/home/user/project",
	})
	if err != nil {
		t.Fatalf("expected DB row to remain, got error: %v", err)
	}
	if got != "idle" {
		t.Errorf("status = %q, want %q", got, "idle")
	}
}

func TestRun_CustomName(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	tmux.sessions["muxac-foo@home@user@project"] = true
	tmux.sessions["muxac-foo@home@user@other"] = true
	queries := database.SetupTestDB(t)
	seedSession(t, queries, "foo", "/home/user/project")
	seedSession(t, queries, "foo", "/home/user/other")

	err := remove.Run(ctx, tmux, queries, "foo", "/home/user/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.killed) != 1 || tmux.killed[0] != "muxac-foo@home@user@project" {
		t.Errorf("expected kill for muxac-foo@home@user@project, got %v", tmux.killed)
	}

	_, err = queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "foo",
		Path: "/home/user/project",
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows for foo at /home/user/project, got %v", err)
	}

	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "foo",
		Path: "/home/user/other",
	})
	if err != nil {
		t.Fatalf("expected row at /home/user/other to remain, got error: %v", err)
	}
	if got != "idle" {
		t.Errorf("foo@/home/user/other status = %q, want %q", got, "idle")
	}
}

func TestRun_KillFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	tmux.sessions["muxac-default@home@user@project"] = true
	tmux.killError = errors.New("boom")
	queries := database.SetupTestDB(t)
	seedSession(t, queries, "default", "/home/user/project")

	err := remove.Run(ctx, tmux, queries, "default", "/home/user/project")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want to contain %q", err, "boom")
	}

	got, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: "default",
		Path: "/home/user/project",
	})
	if err != nil {
		t.Fatalf("expected DB row to remain, got error: %v", err)
	}
	if got != "idle" {
		t.Errorf("status = %q, want %q", got, "idle")
	}
}
