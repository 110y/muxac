package remove

import (
	"context"
	"fmt"

	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/pathkey"
	"github.com/110y/muxac/internal/tmux"
)

// Run executes the remove command. It kills the tmux session identified by
// (name, workDir), then deletes the corresponding row in the sessions table.
// Returns an error if the session does not exist in tmux.
func Run(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, name, workDir string) error {
	sessionName := pathkey.TmuxSessionName(name, workDir)

	if !tmuxRunner.HasSession(ctx, sessionName) {
		return fmt.Errorf("session %q does not exist for %s", name, workDir)
	}

	if err := tmuxRunner.KillSession(ctx, sessionName); err != nil {
		return err
	}

	return queries.DeleteSession(ctx, sqlc.DeleteSessionParams{
		Name: name,
		Path: workDir,
	})
}
