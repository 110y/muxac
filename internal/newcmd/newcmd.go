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
		Status:    "stopped",
		UpdatedAt: timestamp.Now(),
	}); err != nil {
		return err
	}

	env = append(env, agent.EnvSessionName+"="+name)

	return tmuxRunner.NewSession(ctx, sessionName, env, command, tmuxConf)
}
