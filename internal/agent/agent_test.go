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
			name: "Codex returns codex",
			tool: agent.Codex,
			want: "codex",
		},
		{
			name: "Gemini returns gemini",
			tool: agent.Gemini,
			want: "gemini",
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
		geminiProjectDir string
		claudeProjectDir string
		hookInputCwd     string
		want             agent.Tool
	}{
		{
			name:             "Gemini detected via GEMINI_PROJECT_DIR (cwd empty)",
			geminiProjectDir: "/home/user/project",
			want:             agent.Gemini,
		},
		{
			name:             "Gemini detected when GEMINI_PROJECT_DIR matches cwd",
			geminiProjectDir: "/home/user/project",
			hookInputCwd:     "/home/user/project",
			want:             agent.Gemini,
		},
		{
			name:             "Gemini ignored when GEMINI_PROJECT_DIR mismatches cwd",
			geminiProjectDir: "/home/user/project",
			hookInputCwd:     "/home/user/elsewhere",
			want:             agent.Codex,
		},
		{
			name:             "Gemini takes priority when both env vars set and match",
			geminiProjectDir: "/home/user/project",
			claudeProjectDir: "/home/user/project",
			hookInputCwd:     "/home/user/project",
			want:             agent.Gemini,
		},
		{
			name:             "Claude detected via CLAUDE_PROJECT_DIR (cwd empty)",
			claudeProjectDir: "/home/user/project",
			want:             agent.Claude,
		},
		{
			name:             "Claude detected when CLAUDE_PROJECT_DIR matches cwd",
			claudeProjectDir: "/home/user/project",
			hookInputCwd:     "/home/user/project",
			want:             agent.Claude,
		},
		{
			name:             "Stale CLAUDE_PROJECT_DIR ignored, falls back to Codex via cwd",
			claudeProjectDir: "/home/user/somewhere",
			hookInputCwd:     "/home/user/elsewhere",
			want:             agent.Codex,
		},
		{
			name:         "Codex detected via hook input cwd alone",
			hookInputCwd: "/home/user/project",
			want:         agent.Codex,
		},
		{
			name: "Unknown when everything is empty",
			want: agent.Unknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agent.DetectTool(tt.geminiProjectDir, tt.claudeProjectDir, tt.hookInputCwd)
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
		geminiProjectDir string
		claudeProjectDir string
		hookInputCwd     string
		want             string
	}{
		{
			name:             "Gemini returns hook input cwd",
			tool:             agent.Gemini,
			geminiProjectDir: "/home/user/gemini-project",
			hookInputCwd:     "/home/user/gemini-project",
			want:             "/home/user/gemini-project",
		},
		{
			name:             "Gemini falls back to GEMINI_PROJECT_DIR when cwd empty",
			tool:             agent.Gemini,
			geminiProjectDir: "/home/user/gemini-project",
			want:             "/home/user/gemini-project",
		},
		{
			name:             "Claude returns hook input cwd",
			tool:             agent.Claude,
			claudeProjectDir: "/home/user/project",
			hookInputCwd:     "/home/user/project",
			want:             "/home/user/project",
		},
		{
			name:             "Claude falls back to CLAUDE_PROJECT_DIR when cwd empty",
			tool:             agent.Claude,
			claudeProjectDir: "/home/user/project",
			want:             "/home/user/project",
		},
		{
			name:         "Codex returns hook input cwd",
			tool:         agent.Codex,
			hookInputCwd: "/home/user/codex-project",
			want:         "/home/user/codex-project",
		},
		{
			name:             "Codex returns empty when cwd missing",
			tool:             agent.Codex,
			claudeProjectDir: "/home/user/leaked-from-parent",
			want:             "",
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
			got := agent.ProjectDir(tt.tool, tt.geminiProjectDir, tt.claudeProjectDir, tt.hookInputCwd)
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
			name: "codex returns Codex",
			s:    "codex",
			want: agent.Codex,
		},
		{
			name: "gemini returns Gemini",
			s:    "gemini",
			want: agent.Gemini,
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

func TestGeminiSessionFilePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		homeDir    string
		projectDir string
		sessionID  string
		want       string
	}{
		{
			name:       "valid session ID returns glob pattern",
			homeDir:    "/home/user",
			projectDir: "/home/user/myproject",
			sessionID:  "abcdefgh-1234-5678",
			want:       filepath.Join("/home/user", ".gemini", "tmp", "myproject", "chats", "session-*-abcdefgh.json"),
		},
		{
			name:       "session ID exactly 8 chars",
			homeDir:    "/home/user",
			projectDir: "/home/user/project",
			sessionID:  "12345678",
			want:       filepath.Join("/home/user", ".gemini", "tmp", "project", "chats", "session-*-12345678.json"),
		},
		{
			name:       "session ID too short returns empty",
			homeDir:    "/home/user",
			projectDir: "/home/user/project",
			sessionID:  "short",
			want:       "",
		},
		{
			name:       "empty session ID returns empty",
			homeDir:    "/home/user",
			projectDir: "/home/user/project",
			sessionID:  "",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := agent.GeminiSessionFilePattern(tt.homeDir, tt.projectDir, tt.sessionID)
			if got != tt.want {
				t.Errorf("GeminiSessionFilePattern() = %q, want %q", got, tt.want)
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
			name:      "Gemini BeforeAgent maps to UserPromptSubmit",
			tool:      agent.Gemini,
			rawEvent:  "BeforeAgent",
			wantEvent: "UserPromptSubmit",
		},
		{
			name:      "Gemini BeforeTool maps to PreToolUse",
			tool:      agent.Gemini,
			rawEvent:  "BeforeTool",
			wantEvent: "PreToolUse",
		},
		{
			name:      "Gemini AfterAgent maps to Stop",
			tool:      agent.Gemini,
			rawEvent:  "AfterAgent",
			wantEvent: "Stop",
		},
		{
			name:      "Gemini SessionStart passes through",
			tool:      agent.Gemini,
			rawEvent:  "SessionStart",
			wantEvent: "SessionStart",
		},
		{
			name:      "Gemini SessionEnd passes through",
			tool:      agent.Gemini,
			rawEvent:  "SessionEnd",
			wantEvent: "SessionEnd",
		},
		{
			name:      "Gemini unknown event passes through",
			tool:      agent.Gemini,
			rawEvent:  "PermissionRequest",
			wantEvent: "PermissionRequest",
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
