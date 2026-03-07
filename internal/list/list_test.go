package list

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/status"
	"github.com/110y/muxac/internal/timestamp"
)

func TestSortEntries(t *testing.T) {
	t.Parallel()

	entries := []entry{
		{name: "zproject", path: "/home/user/b"},
		{name: "aproject", path: "/home/user/b"},
		{name: "mproject", path: "/home/user/a"},
		{name: "bproject", path: "/home/user/a"},
	}

	sortEntries(entries)

	expected := []struct {
		name string
		path string
	}{
		{"bproject", "/home/user/a"},
		{"mproject", "/home/user/a"},
		{"aproject", "/home/user/b"},
		{"zproject", "/home/user/b"},
	}

	for i, e := range expected {
		if entries[i].name != e.name || entries[i].path != e.path {
			t.Errorf("entry[%d] = {%q, %q}, want {%q, %q}", i, entries[i].name, entries[i].path, e.name, e.path)
		}
	}
}

func TestFormatEntries(t *testing.T) {
	t.Parallel()

	t.Run("with header and all columns", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		entries := []entry{
			{name: "default", status: status.Running, path: "/home/user/project"},
		}
		formatEntries(&buf, entries, Options{})
		output := buf.String()

		if !strings.Contains(output, "NAME") {
			t.Errorf("expected header with 'NAME', got %q", output)
		}
		if !strings.Contains(output, "STATUS") {
			t.Errorf("expected header with 'STATUS', got %q", output)
		}
		if !strings.Contains(output, "DIRECTORY") {
			t.Errorf("expected header with 'DIRECTORY', got %q", output)
		}
		if !strings.Contains(output, "default") {
			t.Errorf("expected entry name 'default', got %q", output)
		}
	})

	t.Run("no header", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		entries := []entry{
			{name: "default", status: status.Running, path: "/home/user/project"},
		}
		formatEntries(&buf, entries, Options{NoHeader: true})
		output := buf.String()

		lines := strings.Split(strings.TrimSpace(output), "\n")
		if len(lines) != 1 {
			t.Errorf("expected 1 line (no header), got %d lines: %q", len(lines), output)
		}
	})

	t.Run("empty entries", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		formatEntries(&buf, nil, Options{})
		if buf.Len() != 0 {
			t.Errorf("expected empty output for no entries, got %q", buf.String())
		}
	})

	t.Run("no color codes present", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		entries := []entry{
			{name: "project", status: status.Running, path: "/tmp"},
		}
		formatEntries(&buf, entries, Options{})
		output := buf.String()

		if strings.Contains(output, "\033[") {
			t.Errorf("expected no ANSI escape codes in output, got %q", output)
		}
	})
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("no sessions in DB", func(t *testing.T) {
		t.Parallel()
		queries := database.SetupTestDB(t)
		var buf bytes.Buffer

		err := Run(t.Context(), &buf, queries, Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if buf.Len() != 0 {
			t.Errorf("expected no output for no sessions, got %q", buf.String())
		}
	})

	t.Run("sessions from DB are displayed", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		queries := database.SetupTestDB(t)
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name:      "default",
			Path:      "/home/user/project",
			Status:    "running",
			UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		err := Run(ctx, &buf, queries, Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		output := buf.String()
		if !strings.Contains(output, "default") {
			t.Errorf("expected output to contain 'default', got %q", output)
		}
		if !strings.Contains(output, "running") {
			t.Errorf("expected output to contain 'running', got %q", output)
		}
		if !strings.Contains(output, "/home/user/project") {
			t.Errorf("expected output to contain path, got %q", output)
		}
	})

	t.Run("all sessions displayed regardless of tmux state", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		queries := database.SetupTestDB(t)
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name:      "alive",
			Path:      "/home/user/project1",
			Status:    "running",
			UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name:      "dead",
			Path:      "/home/user/project2",
			Status:    "stopped",
			UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		err := Run(ctx, &buf, queries, Options{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		output := buf.String()
		if !strings.Contains(output, "alive") {
			t.Errorf("expected output to contain 'alive', got %q", output)
		}
		if !strings.Contains(output, "dead") {
			t.Errorf("expected output to contain 'dead', got %q", output)
		}
	})

	t.Run("output with no-header flag", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		queries := database.SetupTestDB(t)
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name:      "default",
			Path:      "/home/user/project",
			Status:    "stopped",
			UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		err := Run(ctx, &buf, queries, Options{NoHeader: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		output := buf.String()
		lines := strings.Split(strings.TrimSpace(output), "\n")
		if len(lines) != 1 {
			t.Errorf("expected 1 line (no header), got %d lines: %q", len(lines), output)
		}
	})

	t.Run("json output with sessions", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		queries := database.SetupTestDB(t)
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name:      "default",
			Path:      "/home/user/project",
			Status:    "running",
			UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		err := Run(ctx, &buf, queries, Options{JSON: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var got jsonOutput
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("invalid JSON output: %v\nraw: %q", err, buf.String())
		}
		if len(got.Sessions) != 1 {
			t.Fatalf("expected 1 session, got %d", len(got.Sessions))
		}
		s := got.Sessions[0]
		if s.Name != "default" {
			t.Errorf("name = %q, want %q", s.Name, "default")
		}
		if s.Status != "running" {
			t.Errorf("status = %q, want %q", s.Status, "running")
		}
		if s.Directory != "/home/user/project" {
			t.Errorf("directory = %q, want %q", s.Directory, "/home/user/project")
		}
	})

	t.Run("json output with no sessions", func(t *testing.T) {
		t.Parallel()
		queries := database.SetupTestDB(t)

		var buf bytes.Buffer
		err := Run(t.Context(), &buf, queries, Options{JSON: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var got jsonOutput
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("invalid JSON output: %v\nraw: %q", err, buf.String())
		}
		if got.Sessions == nil {
			t.Error("sessions should be empty array, not null")
		}
		if len(got.Sessions) != 0 {
			t.Errorf("expected 0 sessions, got %d", len(got.Sessions))
		}
	})

	t.Run("json output ignores no-header flag", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		queries := database.SetupTestDB(t)
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name:      "default",
			Path:      "/home/user/project",
			Status:    "running",
			UpdatedAt: timestamp.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		err := Run(ctx, &buf, queries, Options{
			JSON:     true,
			NoHeader: true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var got jsonOutput
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("invalid JSON output: %v\nraw: %q", err, buf.String())
		}
		if len(got.Sessions) != 1 {
			t.Fatalf("expected 1 session, got %d", len(got.Sessions))
		}
		s := got.Sessions[0]
		if s.Directory != "/home/user/project" {
			t.Errorf("directory = %q, want %q", s.Directory, "/home/user/project")
		}
		if s.Status != "running" {
			t.Errorf("status = %q, want %q", s.Status, "running")
		}
	})
}
