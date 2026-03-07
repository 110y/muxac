package agent_test

import (
	"path/filepath"
	"testing"

	"github.com/110y/muxac/internal/agent"
)

func TestToolString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tool agent.Tool
		want string
	}{
		{
			name: "Claude returns claude",
			tool: agent.Claude,
			want: "claude",
		},
		{
			name: "Unknown returns unknown",
			tool: agent.Unknown,
			want: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.tool.String()
			if got != tt.want {
				t.Errorf("Tool.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectTool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		claudeProjectDir string
		want             agent.Tool
	}{
		{
			name:             "Claude detected via CLAUDE_PROJECT_DIR",
			claudeProjectDir: "/home/user/project",
			want:             agent.Claude,
		},
		{
			name:             "Unknown when CLAUDE_PROJECT_DIR is empty",
			claudeProjectDir: "",
			want:             agent.Unknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agent.DetectTool(tt.claudeProjectDir)
			if got != tt.want {
				t.Errorf("DetectTool() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProjectDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		tool             agent.Tool
		claudeProjectDir string
		want             string
	}{
		{
			name:             "Claude returns CLAUDE_PROJECT_DIR",
			tool:             agent.Claude,
			claudeProjectDir: "/home/user/project",
			want:             "/home/user/project",
		},
		{
			name:             "Unknown returns empty string",
			tool:             agent.Unknown,
			claudeProjectDir: "/home/user/project",
			want:             "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agent.ProjectDir(tt.tool, tt.claudeProjectDir)
			if got != tt.want {
				t.Errorf("ProjectDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolFromString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    string
		want agent.Tool
	}{
		{
			name: "claude returns Claude",
			s:    "claude",
			want: agent.Claude,
		},
		{
			name: "empty returns Unknown",
			s:    "",
			want: agent.Unknown,
		},
		{
			name: "other returns Unknown",
			s:    "other",
			want: agent.Unknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agent.ToolFromString(tt.s)
			if got != tt.want {
				t.Errorf("ToolFromString(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestJsonlPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		tool       agent.Tool
		homeDir    string
		projectDir string
		sessionID  string
		want       string
	}{
		{
			name:       "Claude returns full path",
			tool:       agent.Claude,
			homeDir:    "/home/user",
			projectDir: "/home/user/project",
			sessionID:  "sess-123",
			want:       filepath.Join("/home/user", ".claude", "projects", "-home-user-project", "sess-123.jsonl"),
		},
		{
			name:       "Unknown returns empty",
			tool:       agent.Unknown,
			homeDir:    "/home/user",
			projectDir: "/home/user/project",
			sessionID:  "sess-123",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agent.JsonlPath(tt.tool, tt.homeDir, tt.projectDir, tt.sessionID)
			if got != tt.want {
				t.Errorf("JsonlPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tool      agent.Tool
		rawEvent  string
		wantEvent string
	}{
		{
			name:      "Claude passes event through",
			tool:      agent.Claude,
			rawEvent:  "UserPromptSubmit",
			wantEvent: "UserPromptSubmit",
		},
		{
			name:      "Claude passes Stop through",
			tool:      agent.Claude,
			rawEvent:  "Stop",
			wantEvent: "Stop",
		},
		{
			name:      "Unknown falls back to Claude conventions",
			tool:      agent.Unknown,
			rawEvent:  "UserPromptSubmit",
			wantEvent: "UserPromptSubmit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotEvent := agent.NormalizeEvent(tt.tool, tt.rawEvent)
			if gotEvent != tt.wantEvent {
				t.Errorf("NormalizeEvent() event = %q, want %q", gotEvent, tt.wantEvent)
			}
		})
	}
}
