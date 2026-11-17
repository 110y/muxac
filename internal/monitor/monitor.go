// Package monitor keeps the rows of the sessions in the database: the server
// of every session watches its session with Watch for as long as it runs, and
// Prune deletes the rows of the sessions whose servers went away without doing
// so.
package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/pathkey"
	"github.com/110y/muxac/internal/status"
	"github.com/110y/muxac/internal/timestamp"
)

// isAfter parses two timestamp strings and returns true if a is strictly after b.
// It tries timestamp.Format first, then falls back to time.RFC3339Nano.
// Returns false if either timestamp cannot be parsed.
func isAfter(a, b string) bool {
	ta, err := parseTimestamp(a)
	if err != nil {
		return false
	}
	tb, err := parseTimestamp(b)
	if err != nil {
		return false
	}
	return ta.After(tb)
}

func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(timestamp.Format, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

type monitorState struct {
	captureClearCount map[string]int
	capturePromptSeen map[string]bool
}

func newMonitorState() *monitorState {
	return &monitorState{
		captureClearCount: make(map[string]int),
		capturePromptSeen: make(map[string]bool),
	}
}

// forget drops every debounce counter kept for the session identified by key.
func (s *monitorState) forget(key string) {
	delete(s.captureClearCount, key)
	delete(s.capturePromptSeen, key)
}

const (
	// syncInterval is how often Watch brings the status of its session up
	// to date.
	syncInterval = time.Second
	// debugLogRetention is how long debug log entries are kept, and
	// debugLogCleanupInterval how often Watch deletes the older ones.
	debugLogRetention       = 7 * 24 * time.Hour
	debugLogCleanupInterval = time.Hour
	// deleteTimeout bounds how long deleting the row of an ended session may
	// take.
	deleteTimeout = 5 * time.Second
)

// Watch keeps the row of the session (name, path) in the database as long as
// ctx is not done, which it is once the session ends, and deletes the row
// then. Every second, it brings the status of the session up to date for the
// agents that do not report it themselves, from what screen returns, which is
// what the session's screen shows, and from the session files of Gemini CLI.
// It also deletes the debug log entries older than a week. Errors are logged
// with logger.
//
// It is what the server of every session runs, so that the session has a row
// as long as it runs, and no longer.
func Watch(ctx context.Context, queries *sqlc.Queries, homeDir, name, path string, screen func() string, logger *slog.Logger) {
	state := newMonitorState()
	deleteOldDebugLog(ctx, queries, logger)
	cleaned := time.Now()

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Deleting the row outlives ctx, which is done by now.
			deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
			if err := queries.DeleteSession(deleteCtx, sqlc.DeleteSessionParams{Name: name, Path: path}); err != nil {
				logger.ErrorContext(deleteCtx, "delete the row of the ended session failed", "error", err)
			}
			cancel()
			return
		case <-ticker.C:
			// Failures caused by the session ending are not worth telling.
			if err := syncNamedSession(ctx, queries, homeDir, name, path, screen, state); err != nil && ctx.Err() == nil {
				logger.ErrorContext(ctx, "sync failed", "error", err)
			}
			if time.Since(cleaned) >= debugLogCleanupInterval {
				deleteOldDebugLog(ctx, queries, logger)
				cleaned = time.Now()
			}
		}
	}
}

func deleteOldDebugLog(ctx context.Context, queries *sqlc.Queries, logger *slog.Logger) {
	threshold := time.Now().Add(-debugLogRetention).UTC().Format(timestamp.Format)
	if err := queries.DeleteOldDebugLog(ctx, threshold); err != nil && ctx.Err() == nil {
		logger.ErrorContext(ctx, "delete old debug log failed", "error", err)
	}
}

// syncNamedSession brings the status of the running session (name, path) up
// to date.
func syncNamedSession(ctx context.Context, queries *sqlc.Queries, homeDir, name, path string, screen func() string, state *monitorState) error {
	row, err := queries.GetSession(ctx, sqlc.GetSessionParams{Name: name, Path: path})
	if errors.Is(err, sql.ErrNoRows) {
		// The row is gone although the session runs, as when Prune ran while
		// the session was starting: it is made again, as `muxac wrap` makes
		// it, since a session without a row is not listed.
		state.forget(name + ":" + path)
		if err := queries.EnsureSession(ctx, sqlc.EnsureSessionParams{
			Name:      name,
			Path:      path,
			Status:    string(status.Idle),
			UpdatedAt: timestamp.Now(),
		}); err != nil {
			return fmt.Errorf("make the row of session %s/%s again: %w", name, path, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get session %s/%s: %w", name, path, err)
	}
	return syncSession(ctx, queries, homeDir, sqlc.ListSessionsRow(row), screen, state)
}

// SessionLister lists the running sessions, as mux.Client does.
type SessionLister interface {
	ListSessionNames(ctx context.Context) ([]string, error)
}

// Prune deletes the rows of the sessions that no longer run but whose servers
// could not delete them, as when they were killed or the machine went down.
// If a server does not answer, Prune fails without deleting anything.
func Prune(ctx context.Context, sessions SessionLister, queries *sqlc.Queries) error {
	// The rows are read before the running sessions are listed, so that the
	// row of a session that starts in between is not taken for one of a
	// session that went away.
	rows, err := queries.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	running, err := sessions.ListSessionNames(ctx)
	if err != nil {
		return fmt.Errorf("list the running sessions: %w", err)
	}
	alive := make(map[string]bool, len(running))
	for _, name := range running {
		alive[name] = true
	}

	var errs []error
	for _, row := range rows {
		if alive[pathkey.SessionName(row.Name, row.Path)] {
			continue
		}
		if err := queries.DeleteSession(ctx, sqlc.DeleteSessionParams{Name: row.Name, Path: row.Path}); err != nil {
			errs = append(errs, fmt.Errorf("delete the row of session %s/%s: %w", row.Name, row.Path, err))
		}
	}
	return errors.Join(errs...)
}

func syncSession(ctx context.Context, queries *sqlc.Queries, homeDir string, sess sqlc.ListSessionsRow, screen func() string, state *monitorState) error {
	// The status of a session whose agent speaks the Program Status Protocol
	// (OSC 7501) is kept in sync by `muxac wrap` from the agent's own reports.
	// The heuristics below only make up for agents without it.
	if sess.ProgramStatus != 0 {
		state.forget(sess.Name + ":" + sess.Path)
		return nil
	}

	switch agent.ToolFromString(sess.AgentTool) {
	case agent.Codex:
		return syncCodexSession(ctx, queries, sess, screen, state)
	case agent.Gemini:
		return syncGeminiSession(ctx, queries, homeDir, sess, screen, state)
	case agent.Claude, agent.Unknown:
		// Claude Code reports its status through OSC 7501, which `muxac wrap`
		// records; there is nothing to infer for it.
		return nil
	}

	return nil
}

// readTail reads the last n bytes from a file. If the file is smaller than n,
// the entire file is returned.
func readTail(filePath string, n int64) ([]byte, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("seek end %q: %w", filePath, err)
	}
	if size == 0 {
		return nil, nil
	}

	readSize := min(n, size)
	offset := size - readSize
	buf := make([]byte, readSize)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil, fmt.Errorf("read tail %q: %w", filePath, err)
	}
	return buf, nil
}

type geminiInfoMessage struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Content   string `json:"content"`
}

func syncGeminiSession(ctx context.Context, queries *sqlc.Queries, homeDir string, sess sqlc.ListSessionsRow, screen func() string, state *monitorState) error {
	st := status.Status(sess.Status)
	sessionKey := sess.Name + ":" + sess.Path

	// File-based cancellation detection.
	if sess.AgentSessionID != "" {
		pattern := agent.GeminiSessionFilePattern(homeDir, sess.Path, sess.AgentSessionID)
		if pattern != "" {
			if cancelled, err := detectGeminiCancellation(ctx, queries, sess, pattern, st); err != nil {
				return err
			} else if cancelled {
				delete(state.captureClearCount, sessionKey)
				delete(state.capturePromptSeen, sessionKey)
				return nil
			}
		}
	}

	// Screen-capture based waiting→running detection.
	if st != status.Waiting {
		delete(state.captureClearCount, sessionKey)
		delete(state.capturePromptSeen, sessionKey)
		return nil
	}

	if terminalShowsGeminiWaitingPrompt(screen()) {
		state.captureClearCount[sessionKey] = 0
		state.capturePromptSeen[sessionKey] = true
		return nil
	}

	if !state.capturePromptSeen[sessionKey] {
		return nil
	}

	state.captureClearCount[sessionKey]++
	if state.captureClearCount[sessionKey] < 2 {
		return nil // debounce: need 2 consecutive clears
	}

	delete(state.captureClearCount, sessionKey)
	delete(state.capturePromptSeen, sessionKey)
	return queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
		Status:    string(status.Running),
		UpdatedAt: timestamp.Now(),
		Name:      sess.Name,
		Path:      sess.Path,
		Status_2:  string(st),
	})
}

// detectGeminiCancellation checks the Gemini session file for a "Request cancelled."
// marker and transitions the session to idle if found. Returns true if a cancellation
// was detected and handled.
func detectGeminiCancellation(ctx context.Context, queries *sqlc.Queries, sess sqlc.ListSessionsRow, pattern string, st status.Status) (bool, error) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false, fmt.Errorf("glob gemini session %q: %w", pattern, err)
	}
	if len(matches) == 0 {
		return false, nil
	}

	// Pick the most recently modified file when multiple matches exist.
	sessionFile := matches[0]
	if len(matches) > 1 {
		var newest time.Time
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil {
				continue
			}
			if fi.ModTime().After(newest) {
				newest = fi.ModTime()
				sessionFile = m
			}
		}
	}
	tail, err := readTail(sessionFile, 4096)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read gemini session tail %q: %w", sessionFile, err)
	}

	const marker = `"Request cancelled."`
	chunk := string(tail)
	idx := strings.LastIndex(chunk, marker)
	if idx < 0 {
		return false, nil
	}

	// Extract the enclosing JSON object {...} around the marker.
	start := strings.LastIndex(chunk[:idx], "{")
	if start < 0 {
		return false, nil
	}
	end := strings.Index(chunk[idx:], "}")
	if end < 0 {
		return false, nil
	}
	objStr := chunk[start : idx+end+1]

	var msg geminiInfoMessage
	if err := json.Unmarshal([]byte(objStr), &msg); err != nil {
		return false, nil
	}

	if msg.Timestamp == "" {
		return false, nil
	}

	if !isAfter(msg.Timestamp, sess.UpdatedAt) {
		return false, nil
	}

	if st != status.Running && st != status.Waiting {
		return false, nil
	}

	if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
		Status:    string(status.Idle),
		UpdatedAt: timestamp.Now(),
		Name:      sess.Name,
		Path:      sess.Path,
		Status_2:  string(st),
	}); err != nil {
		return false, fmt.Errorf("update gemini status to idle: %w", err)
	}
	return true, nil
}

// terminalShowsGeminiWaitingPrompt checks if the captured terminal output contains
// patterns indicating a Gemini CLI permission/approval prompt is visible.
func terminalShowsGeminiWaitingPrompt(output string) bool {
	output = strings.ReplaceAll(output, "\u00a0", " ")

	allLines := strings.Split(output, "\n")
	var nonEmpty []string
	for _, l := range allLines {
		trimmed := strings.TrimSpace(l)
		if trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	if len(nonEmpty) > 15 {
		nonEmpty = nonEmpty[len(nonEmpty)-15:]
	}

	lower := strings.ToLower(strings.Join(nonEmpty, "\n"))

	patterns := []string{
		"allow once",
		"allow for this session",
		"allow for all future sessions",
		"allow for this file in all future sessions",
		"allow this command for all future sessions",
		"allow tool for this session",
		"allow all server tools for this session",
		"allow tool for all future sessions",
		"no, suggest changes",
		"action required",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// syncCodexSession detects user interruptions (Ctrl+C) that Codex does not
// signal via its hook system. Without this, a `running` or `waiting` session
// would remain stuck because no Stop hook fires when the agent is interrupted.
//
// It uses screen-capture heuristics: while the agent is busy, the Codex TUI
// renders "esc to interrupt"; while waiting on approval, the permission
// prompt keywords are visible. If neither indicator is present for two
// consecutive sync ticks, the session is transitioned to idle.
func syncCodexSession(ctx context.Context, queries *sqlc.Queries, sess sqlc.ListSessionsRow, screen func() string, state *monitorState) error {
	st := status.Status(sess.Status)
	sessionKey := sess.Name + ":" + sess.Path

	if st != status.Running && st != status.Waiting {
		delete(state.captureClearCount, sessionKey)
		return nil
	}

	if codexShowsActivity(screen()) {
		state.captureClearCount[sessionKey] = 0
		return nil
	}

	state.captureClearCount[sessionKey]++
	if state.captureClearCount[sessionKey] < 2 {
		return nil
	}
	delete(state.captureClearCount, sessionKey)

	if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
		Status:    string(status.Idle),
		UpdatedAt: timestamp.Now(),
		Name:      sess.Name,
		Path:      sess.Path,
		Status_2:  string(st),
	}); err != nil {
		return fmt.Errorf("update codex status to idle via screen capture: %w", err)
	}
	return nil
}

// codexShowsActivity returns true when the captured Codex TUI indicates the
// agent is still busy or awaiting an approval. The "esc to interrupt" hint
// is rendered only while the agent is processing; permission prompts render
// distinctive question text. Empty output is treated as still-busy because
// the TUI may briefly render nothing during transitions.
func codexShowsActivity(output string) bool {
	output = strings.ReplaceAll(output, " ", " ")
	lower := strings.ToLower(output)

	if strings.TrimSpace(lower) == "" {
		return true
	}

	if strings.Contains(lower, "esc to interrupt") {
		return true
	}

	for _, p := range codexPermissionPromptPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

var codexPermissionPromptPatterns = []string{
	"would you like to run the following command",
	"do you want to approve network access",
	"would you like to grant these permissions",
	"would you like to apply this patch",
}
