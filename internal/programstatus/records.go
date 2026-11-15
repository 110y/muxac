package programstatus

import (
	"strings"

	"github.com/110y/muxac/internal/status"
)

// maxRecords is how many records are kept per program. At capacity, the least
// recently updated record is removed to make room, as the specification asks.
const maxRecords = 256

// Records holds the records a program has reported, following the protocol's
// record semantics: each report replaces the record for its id completely, and
// a clear deletes the targeted record together with every record nested under
// it.
//
// The zero value is ready to use.
type Records struct {
	byID map[string]record
	seq  uint64
}

type record struct {
	state state
	// seq orders records by their last update.
	seq uint64
}

// Apply applies a report.
func (rs *Records) Apply(r Report) {
	if r.state == stateClear {
		rs.clear(r.id)
		return
	}

	if rs.byID == nil {
		rs.byID = make(map[string]record)
	}
	if _, ok := rs.byID[r.id]; !ok && len(rs.byID) >= maxRecords {
		rs.evictOldest()
	}
	rs.seq++
	rs.byID[r.id] = record{state: r.state, seq: rs.seq}
}

// ProcessExited applies the end of the program's process: working and blocked
// records describe activity of a running process, so they are cleared, while
// idle, done and error records remain.
func (rs *Records) ProcessExited() {
	for id, rec := range rs.byID {
		if rec.state == stateWorking || rec.state == stateBlocked {
			delete(rs.byID, id)
		}
	}
}

// Status maps the records to a muxac session status:
//
//   - waiting when any record is blocked: the user has to act (approve a
//     permission, answer a question or sign in) before the program continues;
//   - otherwise running when any record is working;
//   - otherwise idle, which covers idle, done (the turn finished) and error
//     (the turn failed and stopped) as well as having no records at all.
func (rs *Records) Status() status.Status {
	working := false
	for _, rec := range rs.byID {
		switch rec.state {
		case stateBlocked:
			return status.Waiting
		case stateWorking:
			working = true
		case stateIdle, stateDone, stateError, stateClear:
		}
	}
	if working {
		return status.Running
	}
	return status.Idle
}

// clear deletes the record with the given id and every record nested under it,
// or all records when id is empty.
func (rs *Records) clear(id string) {
	if id == "" {
		clear(rs.byID)
		return
	}
	for key := range rs.byID {
		if key == id || strings.HasPrefix(key, id+"/") {
			delete(rs.byID, key)
		}
	}
}

func (rs *Records) evictOldest() {
	oldest := ""
	var oldestSeq uint64
	first := true
	for id, rec := range rs.byID {
		if first || rec.seq < oldestSeq {
			oldest, oldestSeq, first = id, rec.seq, false
		}
	}
	delete(rs.byID, oldest)
}
