package monitor

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/timestamp"
	"github.com/110y/muxac/internal/tmux"
	"github.com/110y/muxac/internal/version"
)

const monitorSessionName = "muxac-monitor"

// monitorBuildID identifies the running monitor binary. It is computed once when
// the process starts, combining the embedded version with the executable's size
// and modification time.
//
// The size+mtime are essential for local development: the embedded version is a
// git commit, which does not change between rebuilds of uncommitted work, so a
// version-only check would leave a stale monitor running the old code (and the
// fix under test would appear to have no effect). Comparing the build ID instead
// makes EnsureRunning restart the monitor whenever the binary on disk differs
// from the one the running monitor was started from. The path is deliberately
// excluded so the same binary reached via different symlinks is not treated as a
// change (which would otherwise cause restart thrashing).
var monitorBuildID = computeBuildID()

func computeBuildID() string {
	exe, err := os.Executable()
	if err != nil {
		return version.Version
	}
	info, err := os.Stat(exe)
	if err != nil {
		return version.Version
	}
	return fmt.Sprintf("%s|%d|%d", version.Version, info.Size(), info.ModTime().UnixNano())
}

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

	return row.Version == monitorBuildID
}
