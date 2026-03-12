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
)

// DetectTool determines which coding tool is running based on
// the tool-specific environment variable values.
func DetectTool(claudeProjectDir string) Tool {
	if claudeProjectDir != "" {
		return Claude
	}
	return Unknown
}

// ProjectDir returns the project directory for the given tool.
func ProjectDir(tool Tool, claudeProjectDir string) string {
	switch tool {
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
	case Unknown:
		return ""
	}
	return ""
}

// CodexSessionLogPath returns the file path for a Codex TUI session log.
func CodexSessionLogPath(cacheDir, tmuxSessionName string) string {
	return filepath.Join(cacheDir, "codex", "sessions", tmuxSessionName+".jsonl")
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
	case Unknown:
		// Fall back to Claude conventions for unknown tools.
		return rawEvent
	}
	return rawEvent
}
