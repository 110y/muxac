package newcmd

import (
	"context"
	"fmt"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/pathkey"
	"github.com/110y/muxac/internal/timestamp"
	"github.com/110y/muxac/internal/tmux"
)

// Run executes the new command. It creates a new tmux session identified by (name, workDir).
// Returns an error if a session with the same identity already exists.
func Run(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, name, workDir, tmuxConf, command string, env []string) error {
	sessionName := pathkey.TmuxSessionName(name, workDir)

	if tmuxRunner.HasSession(ctx, sessionName) {
		return fmt.Errorf("session %q already exists for %s", name, workDir)
	}

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name:      name,
		Path:      workDir,
		Status:    "idle",
		UpdatedAt: timestamp.Now(),
	}); err != nil {
		return err
	}

	env = append(env, agent.EnvSessionName+"="+name)

	return tmuxRunner.NewSession(ctx, sessionName, env, stripInheritedAgentSessionEnv(command), tmuxConf)
}

// inheritedAgentSessionEnvVars are variables that Claude Code sets for its own
// process and passes down to its children. When the muxac tmux server is
// (re)started from inside a Claude Code session, the server captures these and
// injects them into every pane it later spawns. A freshly launched Claude Code
// then sees CLAUDECODE / CLAUDE_CODE_SESSION_ID already set, treats itself as a
// nested, already-identified session, and stops persisting its transcript to
// ~/.claude/projects/<dir>/<session-id>.jsonl, so the session cannot be resumed.
var inheritedAgentSessionEnvVars = []string{
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDECODE",
	"CLAUDE_PROJECT_DIR",
}

// stripInheritedAgentSessionEnv wraps command so the spawned pane unsets the
// inherited session-identity variables before exec, letting each pane start as a
// fresh top-level session. Unsetting them is safe for every agent: Claude Code
// repopulates the correct values for its own run, and a stale CLAUDE_PROJECT_DIR
// otherwise makes muxac misdetect Codex/Gemini sessions as Claude.
func stripInheritedAgentSessionEnv(command string) string {
	if command == "" {
		return command
	}

	prefix := "env"
	for _, v := range inheritedAgentSessionEnvVars {
		prefix += " -u " + v
	}

	return prefix + " " + command
}
