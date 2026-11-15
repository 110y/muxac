// Package programstatus implements the terminal side of the Program Status
// Protocol (OSC 7501, revision 0.4): a program tells its terminal what it is
// doing — idle, working, done, blocked or error — by writing escape sequences
// such as
//
//	ESC ] 7501 ; state=blocked:kind=permission:app=claude-code:msg=<base64> BEL
//
// Claude Code speaks the protocol (since v2.1.295), but only after the terminal
// has answered its support query, and tmux neither answers the query nor keeps
// the reports. muxac therefore reads them on a pseudo-terminal of its own that
// sits between tmux and the agent (see package wrap).
//
// Specification: https://www.superlogical.com/rex/docs/build/program-status
package programstatus

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

// state is the state a program reports for one of its records.
type state string

const (
	// stateIdle means the program is at rest, ready for the user's next
	// instruction.
	stateIdle state = "idle"
	// stateWorking means the program is busy.
	stateWorking state = "working"
	// stateDone means a step finished and its result is ready.
	stateDone state = "done"
	// stateBlocked means the program cannot continue until the user acts:
	// approve a permission, answer a question or sign in.
	stateBlocked state = "blocked"
	// stateError means the program failed and stopped.
	stateError state = "error"
	// stateClear is a command rather than a state: it deletes the targeted
	// record and every record nested under it, or all records when the report
	// has no id.
	stateClear state = "clear"
)

// Report is a validated program status report.
type Report struct {
	state state
	// id names the record the report replaces. The empty string is the root
	// record, which describes the program itself; other ids are child records
	// (Claude Code reports one per running background agent).
	id string
}

// Limits from the specification. A report that breaks any of them is dropped
// as a whole.
const (
	maxSequenceBytes  = 4096
	maxKeyBytes       = 16
	maxMsgEncoded     = 2732
	maxMsgDecoded     = 2048
	maxTitleEncoded   = 256
	maxTitleDecoded   = 192
	maxAppBytes       = 32
	maxIDBytes        = 128
	maxIDSegmentBytes = 32
	maxIDDepth        = 8
)

// parseReport parses the payload of a report — the text between "7501;" and
// the string terminator — and validates it against the specification.
//
// Pairs that are malformed (no "=", an empty key, or a byte outside the allowed
// set) are skipped and the remaining pairs still count. The whole report is
// dropped, and ok is false, when the specification requires it: a missing or
// unknown state, a malformed id, an undecodable msg or title, or a broken limit.
func parseReport(payload string) (Report, bool) {
	pairs := make(map[string]string)
	for item := range strings.SplitSeq(payload, ":") {
		key, value, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		key = strings.Trim(key, " ")
		value = strings.Trim(value, " ")
		if !isKey(key) || !isValue(value) {
			continue
		}
		if len(key) > maxKeyBytes {
			return Report{}, false
		}
		// A repeated key takes its last value.
		pairs[key] = value
	}

	var r Report

	switch st := state(pairs["state"]); st {
	case stateIdle, stateWorking, stateDone, stateBlocked, stateError, stateClear:
		r.state = st
	default:
		// An unknown state must not degrade into a known one, so the report is
		// dropped rather than treated as idle.
		return Report{}, false
	}

	if id, ok := pairs["id"]; ok {
		if !isID(id) {
			return Report{}, false
		}
		r.id = id
	}

	if msg, ok := pairs["msg"]; ok && !isText(msg, maxMsgEncoded, maxMsgDecoded) {
		return Report{}, false
	}
	if title, ok := pairs["title"]; ok && !isText(title, maxTitleEncoded, maxTitleDecoded) {
		return Report{}, false
	}
	if app, ok := pairs["app"]; ok && len(app) > maxAppBytes {
		return Report{}, false
	}

	return r, true
}

// isKey reports whether s is a well-formed key: one or more lowercase letters.
func isKey(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// isValue reports whether s only holds bytes allowed in a value: letters,
// digits and "_ . , + / = -".
func isValue(s string) bool {
	for i := range len(s) {
		if !isAlnum(s[i]) && strings.IndexByte("_.,+/=-", s[i]) < 0 {
			return false
		}
	}
	return true
}

// isID reports whether s is a well-formed record id: one to eight
// "/"-separated segments of 1–32 bytes from [A-Za-z0-9_.+-], 128 bytes at most.
func isID(s string) bool {
	if s == "" || len(s) > maxIDBytes {
		return false
	}
	depth := 0
	for segment := range strings.SplitSeq(s, "/") {
		depth++
		if depth > maxIDDepth || segment == "" || len(segment) > maxIDSegmentBytes {
			return false
		}
		for i := range len(segment) {
			if !isAlnum(segment[i]) && strings.IndexByte("_.+-", segment[i]) < 0 {
				return false
			}
		}
	}
	return true
}

// isText reports whether s is valid base64-encoded text within the given
// encoded and decoded size limits. Padding is optional, and the decoded text
// must not contain C0 or C1 control characters or DEL.
func isText(s string, maxEncoded, maxDecoded int) bool {
	if len(s) > maxEncoded {
		return false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(decoded) > maxDecoded {
		return false
	}
	for i := 0; i < len(decoded); {
		r, size := utf8.DecodeRune(decoded[i:])
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
		i += size
	}
	return true
}

func isAlnum(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}
