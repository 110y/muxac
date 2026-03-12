package newcmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/pathkey"
	"github.com/110y/muxac/internal/timestamp"
	"github.com/110y/muxac/internal/tmux"
)

// Run executes the new command. It creates a new tmux session identified by (name, workDir).
// Returns an error if a session with the same identity already exists.
func Run(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, name, workDir, tmuxConf, command, cacheDir string, env []string) error {
	sessionName := pathkey.TmuxSessionName(name, workDir)

	if tmuxRunner.HasSession(ctx, sessionName) {
		return fmt.Errorf("session %q already exists for %s", name, workDir)
	}

	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name:      name,
		Path:      workDir,
		Status:    "stopped",
		UpdatedAt: timestamp.Now(),
	}); err != nil {
		return err
	}

	codexLogPath := agent.CodexSessionLogPath(cacheDir, sessionName)
	if err := os.MkdirAll(filepath.Dir(codexLogPath), 0o755); err != nil {
		return err
	}

	env = append(env, agent.EnvSessionName+"="+name)
	env = append(env, "CODEX_TUI_RECORD_SESSION=1")
	env = append(env, "CODEX_TUI_SESSION_LOG_PATH="+codexLogPath)

	return tmuxRunner.NewSession(ctx, sessionName, env, command, tmuxConf)
}
