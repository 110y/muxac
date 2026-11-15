package wrap

import (
	"context"

	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/status"
	"github.com/110y/muxac/internal/timestamp"
)

// EnsureSession makes sure the row of the session identified by name and path
// exists, without changing it if it does, so that the status the wrapped
// command reports can always be recorded.
func EnsureSession(ctx context.Context, queries *sqlc.Queries, name, path string) error {
	return queries.EnsureSession(ctx, sqlc.EnsureSessionParams{
		Name:      name,
		Path:      path,
		Status:    string(status.Idle),
		UpdatedAt: timestamp.Now(),
	})
}

// RecordStatus records st, reported by the wrapped command, as the status of
// the session identified by name and path. It also marks the session as driven
// by program status reports, so that hook events and the monitor's heuristics
// leave it alone from then on.
func RecordStatus(ctx context.Context, queries *sqlc.Queries, name, path string, st status.Status) error {
	return queries.UpdateSessionProgramStatus(ctx, sqlc.UpdateSessionProgramStatusParams{
		Status:    string(st),
		UpdatedAt: timestamp.Now(),
		Name:      name,
		Path:      path,
	})
}
