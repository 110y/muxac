package attach_test

import (
	"context"
	"testing"

	"github.com/110y/muxac/internal/attach"
)

type fakeTmux struct {
	sessions map[string]bool
	attached []string
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{
		sessions: make(map[string]bool),
	}
}

func (f *fakeTmux) HasSession(_ context.Context, sessionName string) bool {
	return f.sessions[sessionName]
}

func (f *fakeTmux) AttachSession(_ context.Context, sessionName string) error {
	f.attached = append(f.attached, sessionName)
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

func (f *fakeTmux) KillSession(_ context.Context, _ string) error {
	return nil
}

func (f *fakeTmux) NewDetachedSession(_ context.Context, _ string, _ string) error {
	return nil
}

func TestRun_ExistingSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	tmux.sessions["muxac-default@home@user@project"] = true

	err := attach.Run(ctx, tmux, "default", "/home/user/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.attached) != 1 || tmux.attached[0] != "muxac-default@home@user@project" {
		t.Errorf("expected attach for muxac-default@home@user@project, got %v", tmux.attached)
	}
}

func TestRun_NonExistentSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()

	err := attach.Run(ctx, tmux, "default", "/home/user/project")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if len(tmux.attached) != 0 {
		t.Errorf("expected no attach calls, got %v", tmux.attached)
	}
}

func TestRun_CustomName(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	tmux := newFakeTmux()
	tmux.sessions["muxac-foo@home@user@project"] = true

	err := attach.Run(ctx, tmux, "foo", "/home/user/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tmux.attached) != 1 || tmux.attached[0] != "muxac-foo@home@user@project" {
		t.Errorf("expected attach for muxac-foo@home@user@project, got %v", tmux.attached)
	}
}
