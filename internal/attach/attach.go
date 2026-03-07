package attach

import (
	"context"
	"fmt"

	"github.com/110y/muxac/internal/pathkey"
	"github.com/110y/muxac/internal/tmux"
)

// Run executes the attach command. It attaches to an existing tmux session
// identified by (name, workDir). Returns an error if the session does not exist.
func Run(ctx context.Context, tmuxRunner tmux.Runner, name, workDir string) error {
	sessionName := pathkey.TmuxSessionName(name, workDir)

	if !tmuxRunner.HasSession(ctx, sessionName) {
		return fmt.Errorf("session %q does not exist for %s", name, workDir)
	}

	return tmuxRunner.AttachSession(ctx, sessionName)
}
