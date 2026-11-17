package attach

import (
	"context"
	"fmt"

	"github.com/110y/muxac/internal/mux"
	"github.com/110y/muxac/internal/pathkey"
)

// Run executes the attach command. It attaches to an existing session
// identified by (name, workDir). Returns an error if the session does not exist.
func Run(ctx context.Context, runner mux.Runner, name, workDir string) error {
	sessionName := pathkey.SessionName(name, workDir)

	if !runner.HasSession(ctx, sessionName) {
		return fmt.Errorf("session %q does not exist for %s", name, workDir)
	}

	return runner.AttachSession(ctx, sessionName)
}
