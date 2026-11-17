package remove

import (
	"context"
	"fmt"

	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/mux"
	"github.com/110y/muxac/internal/pathkey"
)

// Run executes the remove command. It kills the session identified by
// (name, workDir), then deletes the corresponding row in the sessions table.
// Returns an error if the session does not exist.
func Run(ctx context.Context, runner mux.Runner, queries *sqlc.Queries, name, workDir string) error {
	sessionName := pathkey.SessionName(name, workDir)

	if !runner.HasSession(ctx, sessionName) {
		return fmt.Errorf("session %q does not exist for %s", name, workDir)
	}

	if err := runner.KillSession(ctx, sessionName); err != nil {
		return err
	}

	return queries.DeleteSession(ctx, sqlc.DeleteSessionParams{
		Name: name,
		Path: workDir,
	})
}
