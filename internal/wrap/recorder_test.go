package wrap

import (
	"testing"

	"github.com/110y/muxac/internal/database"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/status"
	"github.com/110y/muxac/internal/timestamp"
)

const (
	testName = "default"
	testPath = "/home/user/project"
)

func TestRecordStatus(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	// The row `muxac new` created, which hook events have been updating.
	if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
		Name: testName, Path: testPath, Status: "running", UpdatedAt: timestamp.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := EnsureSession(ctx, queries, testName, testPath); err != nil {
		t.Fatal(err)
	}
	row := getSession(t, queries)
	if row.Status != "running" || row.ProgramStatus != 0 {
		t.Fatalf("EnsureSession changed an existing session: status=%q program_status=%d", row.Status, row.ProgramStatus)
	}

	if err := RecordStatus(ctx, queries, testName, testPath, status.Waiting); err != nil {
		t.Fatal(err)
	}
	row = getSession(t, queries)
	if row.Status != "waiting" || row.ProgramStatus != 1 {
		t.Errorf("after RecordStatus(waiting): status=%q program_status=%d, want waiting/1", row.Status, row.ProgramStatus)
	}

	if err := RecordStatus(ctx, queries, testName, testPath, status.Idle); err != nil {
		t.Fatal(err)
	}
	row = getSession(t, queries)
	if row.Status != "idle" || row.ProgramStatus != 1 {
		t.Errorf("after RecordStatus(idle): status=%q program_status=%d, want idle/1", row.Status, row.ProgramStatus)
	}
}

func TestEnsureSessionCreatesMissingSession(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	if err := EnsureSession(ctx, queries, testName, testPath); err != nil {
		t.Fatal(err)
	}
	row := getSession(t, queries)
	if row.Status != "idle" || row.ProgramStatus != 0 {
		t.Errorf("created session: status=%q program_status=%d, want idle/0", row.Status, row.ProgramStatus)
	}

	if err := RecordStatus(ctx, queries, testName, testPath, status.Running); err != nil {
		t.Fatal(err)
	}
	if row := getSession(t, queries); row.Status != "running" || row.ProgramStatus != 1 {
		t.Errorf("after RecordStatus(running): status=%q program_status=%d, want running/1", row.Status, row.ProgramStatus)
	}
}

func TestRecordStatusLeavesRemovedSessionAlone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	queries := database.SetupTestDB(t)

	// `muxac remove` deletes the row while the command is shutting down; a
	// late report must not bring the session back.
	if err := RecordStatus(ctx, queries, testName, testPath, status.Idle); err != nil {
		t.Fatal(err)
	}
	rows, err := queries.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("sessions = %d, want 0", len(rows))
	}
}

func getSession(t *testing.T, queries *sqlc.Queries) sqlc.ListSessionsRow {
	t.Helper()

	rows, err := queries.ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("sessions = %d, want 1", len(rows))
	}
	return rows[0]
}
