package attach_test

import (
	"context"
	"testing"

	"github.com/110y/muxac/internal/attach"
)

type fakeMux struct {
	sessions map[string]bool
	attached []string
}

func newFakeMux() *fakeMux {
	return &fakeMux{
		sessions: make(map[string]bool),
	}
}

func (f *fakeMux) HasSession(_ context.Context, sessionName string) bool {
	return f.sessions[sessionName]
}

func (f *fakeMux) AttachSession(_ context.Context, sessionName string) error {
	f.attached = append(f.attached, sessionName)
	return nil
}

func (f *fakeMux) NewSession(_ context.Context, _, _ string, _ []string, _ string) error {
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

func TestRun_ExistingSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()
	fm.sessions["muxac-default@home@user@project"] = true

	err := attach.Run(ctx, fm, "default", "/home/user/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.attached) != 1 || fm.attached[0] != "muxac-default@home@user@project" {
		t.Errorf("expected attach for muxac-default@home@user@project, got %v", fm.attached)
	}
}

func TestRun_NonExistentSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()

	err := attach.Run(ctx, fm, "default", "/home/user/project")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if len(fm.attached) != 0 {
		t.Errorf("expected no attach calls, got %v", fm.attached)
	}
}

func TestRun_CustomName(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	fm := newFakeMux()
	fm.sessions["muxac-foo@home@user@project"] = true

	err := attach.Run(ctx, fm, "foo", "/home/user/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(fm.attached) != 1 || fm.attached[0] != "muxac-foo@home@user@project" {
		t.Errorf("expected attach for muxac-foo@home@user@project, got %v", fm.attached)
	}
}
