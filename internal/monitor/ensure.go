package monitor

import (
	"context"
	"os"
	"time"

	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/timestamp"
	"github.com/110y/muxac/internal/tmux"
	"github.com/110y/muxac/internal/version"
)

const monitorSessionName = "muxac-monitor"

// EnsureRunning ensures the monitor is running in a dedicated tmux session.
// If the monitor session exists with a fresh heartbeat, it returns immediately.
// If the session is stale or missing, it (re)starts the monitor.
func EnsureRunning(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries) error {
	if tmuxRunner.HasSession(ctx, monitorSessionName) {
		if isMonitorAliveAndCurrent(ctx, queries) {
			return nil
		}
		if err := tmuxRunner.KillSession(ctx, monitorSessionName); err != nil {
			return err
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}

	return tmuxRunner.NewDetachedSession(ctx, monitorSessionName, exe+" monitor")
}

func isMonitorAliveAndCurrent(ctx context.Context, queries *sqlc.Queries) bool {
	row, err := queries.GetMonitorHeartbeat(ctx)
	if err != nil {
		return false
	}

	t, err := time.Parse(timestamp.Format, row.UpdatedAt)
	if err != nil {
		return false
	}

	if time.Since(t) >= 10*time.Second {
		return false
	}

	return row.Version == version.Version
}
