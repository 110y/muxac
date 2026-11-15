package programstatus

import (
	"strconv"
	"testing"

	"github.com/110y/muxac/internal/status"
)

func TestRecordsStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		reports []Report
		want    status.Status
	}{
		{
			name: "no records is idle",
			want: status.Idle,
		},
		{
			name:    "root idle",
			reports: []Report{{state: stateIdle}},
			want:    status.Idle,
		},
		{
			name:    "root working is running",
			reports: []Report{{state: stateWorking}},
			want:    status.Running,
		},
		{
			name:    "root blocked on a permission is waiting",
			reports: []Report{{state: stateWorking}, {state: stateBlocked}},
			want:    status.Waiting,
		},
		{
			name:    "root blocked on sign-in is waiting",
			reports: []Report{{state: stateBlocked}},
			want:    status.Waiting,
		},
		{
			name:    "root done is idle",
			reports: []Report{{state: stateWorking}, {state: stateDone}},
			want:    status.Idle,
		},
		{
			name:    "root error is idle",
			reports: []Report{{state: stateWorking}, {state: stateError}},
			want:    status.Idle,
		},
		{
			name:    "approved permission returns to running",
			reports: []Report{{state: stateWorking}, {state: stateBlocked}, {state: stateWorking}},
			want:    status.Running,
		},
		{
			name:    "interrupted turn is idle",
			reports: []Report{{state: stateWorking}, {state: stateIdle}},
			want:    status.Idle,
		},
		{
			name:    "working child keeps an idle root running",
			reports: []Report{{state: stateDone}, {state: stateWorking, id: "agent1"}},
			want:    status.Running,
		},
		{
			name:    "blocked child makes the session waiting",
			reports: []Report{{state: stateWorking}, {state: stateBlocked, id: "agent1"}},
			want:    status.Waiting,
		},
		{
			name:    "cleared child no longer counts",
			reports: []Report{{state: stateDone}, {state: stateWorking, id: "agent1"}, {state: stateClear, id: "agent1"}},
			want:    status.Idle,
		},
		{
			name: "clear removes nested records",
			reports: []Report{
				{state: stateIdle},
				{state: stateWorking, id: "build"},
				{state: stateBlocked, id: "build/test"},
				{state: stateWorking, id: "buildx"},
				{state: stateClear, id: "build"},
			},
			want: status.Running, // only "buildx" is left working
		},
		{
			name:    "clear without id removes everything",
			reports: []Report{{state: stateBlocked}, {state: stateWorking, id: "agent1"}, {state: stateClear}},
			want:    status.Idle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rs Records
			for _, r := range tt.reports {
				rs.Apply(r)
			}
			if got := rs.Status(); got != tt.want {
				t.Errorf("Status() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRecordsProcessExited(t *testing.T) {
	t.Parallel()

	var rs Records
	rs.Apply(Report{state: stateBlocked})
	rs.Apply(Report{state: stateWorking, id: "agent1"})
	rs.Apply(Report{state: stateError, id: "agent2"})
	if got := rs.Status(); got != status.Waiting {
		t.Fatalf("Status() before exit = %q, want %q", got, status.Waiting)
	}

	rs.ProcessExited()

	if got := rs.Status(); got != status.Idle {
		t.Errorf("Status() after exit = %q, want %q", got, status.Idle)
	}
	if _, ok := rs.byID["agent2"]; !ok {
		t.Error("error record was removed by process exit, want it kept")
	}
}

func TestRecordsEvictsLeastRecentlyUpdated(t *testing.T) {
	t.Parallel()

	var rs Records
	rs.Apply(Report{state: stateBlocked, id: "oldest"})
	for i := range maxRecords - 1 {
		rs.Apply(Report{state: stateIdle, id: "r" + strconv.Itoa(i)})
	}
	if got := rs.Status(); got != status.Waiting {
		t.Fatalf("Status() at capacity = %q, want %q", got, status.Waiting)
	}

	rs.Apply(Report{state: stateIdle, id: "newest"})

	if len(rs.byID) != maxRecords {
		t.Errorf("records = %d, want %d", len(rs.byID), maxRecords)
	}
	if _, ok := rs.byID["oldest"]; ok {
		t.Error("least recently updated record was kept, want it evicted")
	}
	if got := rs.Status(); got != status.Idle {
		t.Errorf("Status() after eviction = %q, want %q", got, status.Idle)
	}
}
