package agent

import (
	"path/filepath"

	"github.com/110y/muxac/internal/pathkey"
)

// EnvSessionName is the environment variable used to pass the muxac session name.
const EnvSessionName = "MUXAC_SESSION_NAME"

// Tool represents a supported agentic coding tool.
type Tool int

const (
	Unknown Tool = iota
	Claude
	Codex
	Gemini
)

// DetectTool determines which coding tool is running based on
// the tool-specific environment variable values.
// Gemini CLI sets both GEMINI_PROJECT_DIR and CLAUDE_PROJECT_DIR,
// so it must be checked first.
func DetectTool(geminiProjectDir, claudeProjectDir string) Tool {
	if geminiProjectDir != "" {
		return Gemini
	}
	if claudeProjectDir != "" {
		return Claude
	}
	return Unknown
}

// ProjectDir returns the project directory for the given tool.
func ProjectDir(tool Tool, geminiProjectDir, claudeProjectDir string) string {
	switch tool {
	case Gemini:
		return geminiProjectDir
	case Claude:
		return claudeProjectDir
	case Codex:
		return ""
	case Unknown:
		return ""
	}
	return ""
}

// String returns the canonical name of the tool.
func (t Tool) String() string {
	switch t {
	case Claude:
		return "claude"
	case Codex:
		return "codex"
	case Gemini:
		return "gemini"
	case Unknown:
		return "unknown"
	}
	return "unknown"
}

// ToolFromString converts a database string back to a Tool value.
// Empty or unrecognized strings return Unknown.
func ToolFromString(s string) Tool {
	switch s {
	case "claude":
		return Claude
	case "codex":
		return Codex
	case "gemini":
		return Gemini
	default:
		return Unknown
	}
}

// JsonlPath returns the tool-specific JSONL file path.
// Returns "" for tools that do not support JSONL.
func JsonlPath(tool Tool, homeDir, projectDir, sessionID string) string {
	switch tool {
	case Claude:
		return filepath.Join(homeDir, ".claude", "projects", pathkey.ClaudeProjectDir(projectDir), sessionID+".jsonl")
	case Codex:
		return ""
	case Gemini:
		return ""
	case Unknown:
		return ""
	}
	return ""
}

// CodexSessionLogPath returns the file path for a Codex TUI session log.
func CodexSessionLogPath(cacheDir, tmuxSessionName string) string {
	return filepath.Join(cacheDir, "codex", "sessions", tmuxSessionName+".jsonl")
}

// GeminiSessionFilePattern returns a glob pattern that matches the Gemini CLI
// session JSON file for the given project directory and session ID.
// Returns "" if sessionID is too short (< 8 characters).
func GeminiSessionFilePattern(homeDir, projectDir, sessionID string) string {
	if len(sessionID) < 8 {
		return ""
	}
	return filepath.Join(homeDir, ".gemini", "tmp", filepath.Base(projectDir), "chats", "session-*-"+sessionID[:8]+".json")
}

// NormalizeEvent maps a tool-specific hook event name to the canonical event
// name used by the status package. For Claude, events are already canonical.
// For Unknown tools, Claude conventions are used as a fallback.
func NormalizeEvent(tool Tool, rawEvent string) string {
	switch tool {
	case Claude:
		return rawEvent
	case Codex:
		return rawEvent
	case Gemini:
		switch rawEvent {
		case "BeforeAgent":
			return "UserPromptSubmit"
		case "BeforeTool":
			return "PreToolUse"
		case "AfterAgent":
			return "Stop"
		case "SessionStart":
			return "SessionStart"
		case "SessionEnd":
			return "SessionEnd"
		default:
			return rawEvent
		}
	case Unknown:
		// Fall back to Claude conventions for unknown tools.
		return rawEvent
	}
	return rawEvent
}
