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

// DetectTool determines which coding tool is invoking the hook.
//
// The hook payload's "cwd" is the source of truth for the project directory
// because env vars can leak from a parent shell: e.g. launching `codex` from
// a Claude Code session inherits CLAUDE_PROJECT_DIR, which would otherwise
// make us misdetect Codex hook events as Claude.
//
// Detection rules:
//  1. GEMINI_PROJECT_DIR matches the hook cwd (or cwd is empty) → Gemini.
//     Gemini CLI sets both GEMINI_PROJECT_DIR and CLAUDE_PROJECT_DIR, so this
//     must be checked before Claude.
//  2. CLAUDE_PROJECT_DIR matches the hook cwd (or cwd is empty) → Claude.
//  3. cwd is non-empty → Codex. Codex does not set its own project-dir env
//     var on hook commands but always includes "cwd" in the JSON payload.
//  4. Otherwise → Unknown.
func DetectTool(geminiProjectDir, claudeProjectDir, hookInputCwd string) Tool {
	if geminiProjectDir != "" && (hookInputCwd == "" || geminiProjectDir == hookInputCwd) {
		return Gemini
	}
	if claudeProjectDir != "" && (hookInputCwd == "" || claudeProjectDir == hookInputCwd) {
		return Claude
	}
	if hookInputCwd != "" {
		return Codex
	}
	return Unknown
}

// ProjectDir returns the project directory for the given tool.
// The hook payload's "cwd" is preferred over env vars because env vars can
// leak from a parent shell and point at the wrong project.
func ProjectDir(tool Tool, geminiProjectDir, claudeProjectDir, hookInputCwd string) string {
	if hookInputCwd != "" {
		return hookInputCwd
	}
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
