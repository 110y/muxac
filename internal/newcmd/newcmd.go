package newcmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/pathkey"
	"github.com/110y/muxac/internal/status"
	"github.com/110y/muxac/internal/timestamp"
	"github.com/110y/muxac/internal/tmux"
)

// Run executes the new command. It creates a new tmux session identified by (name, workDir).
// Returns an error if a session with the same identity already exists.
//
// exe is the path of the muxac executable. The pane runs the command through
// `muxac wrap`, which puts it on a pseudo-terminal of its own so that muxac can
// receive the status the agent reports through escape sequences (see package
// wrap).
func Run(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, exe, name, workDir, tmuxConf, command string, env []string) error {
	sessionName := pathkey.TmuxSessionName(name, workDir)

	if tmuxRunner.HasSession(ctx, sessionName) {
		return fmt.Errorf("session %q already exists for %s", name, workDir)
	}

	if err := queries.InitSessionStatus(ctx, sqlc.InitSessionStatusParams{
		Name:      name,
		Path:      workDir,
		Status:    string(status.Idle),
		UpdatedAt: timestamp.Now(),
	}); err != nil {
		return err
	}

	env = append(env, agent.EnvSessionName+"="+name)

	return tmuxRunner.NewSession(ctx, sessionName, env, wrapCommand(exe, name, workDir, command), tmuxConf)
}

// wrapCommand returns the shell command the tmux pane runs: command, stripped of
// inherited agent session variables, under `muxac wrap`. command itself is left
// unquoted so the shell still parses it as before (arguments, quoting and
// VAR=value assignments).
func wrapCommand(exe, name, workDir, command string) string {
	if command == "" {
		return command
	}
	return strings.Join([]string{
		shellQuote(exe), "wrap",
		"--name", shellQuote(name),
		"--dir", shellQuote(workDir),
		"--",
		stripInheritedAgentSessionEnv(command),
	}, " ")
}

// shellQuote quotes s as a single word for the shell tmux runs the pane
// command with.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

	words := []string{"env"}
	for _, v := range inheritedAgentSessionEnvVars {
		words = append(words, "-u", v)
	}

	return strings.Join(append(words, command), " ")
}
