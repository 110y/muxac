package monitor

import (
	"context"
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
	"github.com/110y/muxac/internal/tmux"
	"github.com/110y/muxac/internal/version"
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
	capturePaneClearCount map[string]int
	capturePromptSeen     map[string]bool
}

// Run starts a monitoring loop that syncs session statuses between tmux and the database.
// It runs an initial sync immediately, then repeats every second.
// Returns nil on context cancellation.
func Run(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir, cacheDir string, logger *slog.Logger) error {
	state := &monitorState{
		capturePaneClearCount: make(map[string]int),
		capturePromptSeen:     make(map[string]bool),
	}

	if err := sync(ctx, tmuxRunner, queries, homeDir, cacheDir, state); err != nil {
		logger.ErrorContext(ctx, "sync failed", "error", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := sync(ctx, tmuxRunner, queries, homeDir, cacheDir, state); err != nil {
				logger.ErrorContext(ctx, "sync failed", "error", err)
			}
		}
	}
}

type jsonlContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type jsonlMessage struct {
	Content []jsonlContent `json:"content"`
}

type jsonlLine struct {
	UUID      string       `json:"uuid"`
	Timestamp string       `json:"timestamp"`
	Type      string       `json:"type"`
	Message   jsonlMessage `json:"message"`
}

func isInterruptionLine(line jsonlLine) bool {
	if line.Type != "user" {
		return false
	}
	if len(line.Message.Content) == 0 {
		return false
	}
	c := line.Message.Content[0]
	return c.Type == "text" && strings.HasPrefix(c.Text, "[Request interrupted by user")
}

func sync(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir, cacheDir string, state *monitorState) error {
	var errs []error

	threshold := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(timestamp.Format)
	if err := queries.DeleteOldDebugLog(ctx, threshold); err != nil {
		errs = append(errs, fmt.Errorf("delete old debug log: %w", err))
	}

	if err := queries.UpsertMonitorHeartbeat(ctx, sqlc.UpsertMonitorHeartbeatParams{
		Version:   version.Version,
		UpdatedAt: timestamp.Now(),
	}); err != nil {
		return fmt.Errorf("heartbeat update: %w", err)
	}

	dbSessions, err := queries.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	tmuxSessions, err := tmuxRunner.ListSessionNames(ctx)
	if err != nil {
		return fmt.Errorf("list tmux sessions: %w", err)
	}

	alive := make(map[string]bool, len(tmuxSessions))
	for _, s := range tmuxSessions {
		alive[s] = true
	}

	for _, sess := range dbSessions {
		if err := syncSession(ctx, tmuxRunner, queries, homeDir, cacheDir, sess, alive, state); err != nil {
			errs = append(errs, fmt.Errorf("sync session %s/%s: %w", sess.Name, sess.Path, err))
		}
	}

	return errors.Join(errs...)
}

func syncSession(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir, cacheDir string, sess sqlc.ListSessionsRow, alive map[string]bool, state *monitorState) error {
	tmuxName := pathkey.TmuxSessionName(sess.Name, sess.Path)
	codexLogPath := agent.CodexSessionLogPath(cacheDir, tmuxName)

	if !alive[tmuxName] {
		if err := queries.DeleteSession(ctx, sqlc.DeleteSessionParams{
			Name: sess.Name,
			Path: sess.Path,
		}); err != nil {
			return fmt.Errorf("delete dead session: %w", err)
		}
		os.Remove(codexLogPath)
		delete(state.capturePaneClearCount, sess.Name+":"+sess.Path)
		delete(state.capturePromptSeen, sess.Name+":"+sess.Path)
		return nil
	}

	tool := agent.ToolFromString(sess.AgentTool)

	// Auto-detect Codex by checking if the session log file exists.
	if tool == agent.Unknown {
		if _, err := os.Stat(codexLogPath); err == nil {
			tool = agent.Codex
			if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
				AgentTool: tool.String(),
				UpdatedAt: timestamp.Now(),
				Name:      sess.Name,
				Path:      sess.Path,
			}); err != nil {
				return fmt.Errorf("update agent tool to codex: %w", err)
			}
		}
	}

	switch tool {
	case agent.Codex:
		return syncCodexSession(ctx, queries, cacheDir, sess, tmuxRunner, tmuxName, state)
	case agent.Claude:
		return syncClaudeCodeSession(ctx, queries, homeDir, sess, tmuxRunner, tmuxName, state)
	case agent.Gemini:
		return syncGeminiSession(ctx, queries, homeDir, sess, tmuxRunner, tmuxName, state)
	case agent.Unknown:
		return nil
	}

	return nil
}

// readLastLines reads the last n non-empty lines from a file by seeking
// backward from the end in chunks. It returns os.IsNotExist errors as-is.
func readLastLines(filePath string, n int) ([]string, error) {
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

	const chunkSize = 8192
	var buf []byte
	offset := size
	needed := n + 1 // need n+1 newlines to delimit n lines

	for offset > 0 {
		readSize := min(int64(chunkSize), offset)
		offset -= readSize

		chunk := make([]byte, readSize)
		if _, err := f.ReadAt(chunk, offset); err != nil && err != io.EOF {
			return nil, fmt.Errorf("read chunk %q: %w", filePath, err)
		}

		buf = append(chunk, buf...)

		count := 0
		for _, b := range buf {
			if b == '\n' {
				count++
			}
		}
		if count >= needed {
			break
		}
	}

	lines := strings.Split(string(buf), "\n")

	// Filter empty strings (from leading/trailing newlines).
	result := make([]string, 0, len(lines))
	for _, l := range lines {
		if l != "" {
			result = append(result, l)
		}
	}

	if len(result) > n {
		result = result[len(result)-n:]
	}

	return result, nil
}

func syncClaudeCodeSession(ctx context.Context, queries *sqlc.Queries, homeDir string, sess sqlc.ListSessionsRow, tmuxRunner tmux.Runner, tmuxName string, state *monitorState) error {
	if sess.AgentSessionID == "" {
		return nil
	}

	jsonlPath := agent.JsonlPath(agent.Claude, homeDir, sess.Path, sess.AgentSessionID)

	if jsonlPath != "" {
		lines, err := readLastLines(jsonlPath, 10)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read jsonl tail %q: %w", jsonlPath, err)
		}

		if len(lines) > 0 {
			var lastLine jsonlLine
			var maxTimestamp string
			for _, raw := range lines {
				var line jsonlLine
				if err := json.Unmarshal([]byte(raw), &line); err != nil {
					continue
				}
				lastLine = line
				if line.Timestamp != "" && (maxTimestamp == "" || isAfter(line.Timestamp, maxTimestamp)) {
					maxTimestamp = line.Timestamp
				}
			}

			// Interruption check takes priority over waiting→running.
			if isInterruptionLine(lastLine) && isAfter(lastLine.Timestamp, sess.UpdatedAt) {
				st := status.Status(sess.Status)
				if st == status.Running || st == status.Waiting {
					if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
						Status:       string(status.Idle),
						UpdatedAt:    timestamp.Now(),
						WaitingSince: "",
						Name:         sess.Name,
						Path:         sess.Path,
						Status_2:     string(st),
					}); err != nil {
						return fmt.Errorf("update status to idle: %w", err)
					}
				}
				return nil
			}

			st := status.Status(sess.Status)
			sessionKey := sess.Name + ":" + sess.Path
			if st == status.Waiting && sess.WaitingSince != "" {
				waitingSinceTime, err := parseTimestamp(sess.WaitingSince)
				if err == nil {
					threshold := waitingSinceTime.Add(2 * time.Second)
					maxTimestampTime, err := parseTimestamp(maxTimestamp)
					if err == nil && maxTimestampTime.After(threshold) {
						// Guard: check capture-pane before transitioning.
						// If the permission prompt is still visible, do not revert.
						output, cpErr := tmuxRunner.CapturePane(ctx, tmuxName)
						if cpErr == nil && terminalShowsWaitingPrompt(output) {
							return nil
						}
						if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
							Status:       string(status.Running),
							UpdatedAt:    timestamp.Now(),
							WaitingSince: "",
							Name:         sess.Name,
							Path:         sess.Path,
							Status_2:     string(st),
						}); err != nil {
							return fmt.Errorf("update status to running: %w", err)
						}
						delete(state.capturePaneClearCount, sessionKey)
						return nil
					}
				}
			}
		}
	}

	// Capture-pane based detection: when status is waiting and JSONL shows no new entries,
	// check the terminal content for approval prompt visibility.
	st := status.Status(sess.Status)
	sessionKey := sess.Name + ":" + sess.Path
	if st != status.Waiting {
		delete(state.capturePaneClearCount, sessionKey)
		delete(state.capturePromptSeen, sessionKey)
		return nil
	}
	output, err := tmuxRunner.CapturePane(ctx, tmuxName)
	if err != nil {
		return nil // non-fatal
	}

	if terminalShowsWaitingPrompt(output) {
		state.capturePaneClearCount[sessionKey] = 0
		state.capturePromptSeen[sessionKey] = true
		return nil
	}

	// Only count "prompt gone" if the prompt was previously seen.
	// This prevents false reverts when the prompt hasn't rendered yet
	// or uses text not matching the known patterns.
	if !state.capturePromptSeen[sessionKey] {
		return nil
	}

	state.capturePaneClearCount[sessionKey]++
	if state.capturePaneClearCount[sessionKey] < 2 {
		return nil // debounce: need 2 consecutive clears
	}

	delete(state.capturePaneClearCount, sessionKey)
	if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
		Status:       string(status.Running),
		UpdatedAt:    timestamp.Now(),
		WaitingSince: "",
		Name:         sess.Name,
		Path:         sess.Path,
		Status_2:     string(st),
	}); err != nil {
		return fmt.Errorf("update status to running via capture-pane: %w", err)
	}
	return nil
}

// terminalShowsWaitingPrompt checks if the captured terminal output contains
// patterns indicating a permission/approval prompt is visible.
func terminalShowsWaitingPrompt(output string) bool {
	// Normalize non-breaking spaces (U+00A0) to regular spaces.
	output = strings.ReplaceAll(output, "\u00a0", " ")

	// Take the last 15 non-empty lines.
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
		"yes, allow once",
		"yes, allow always",
		"allow once",
		"allow always",
		"no, and tell claude what to do differently",
		"do you want",
		"would you like",
		"run this command?",
		"execute this?",
		"do you trust the files",
		"use arrow keys to navigate",
		"esc to cancel",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// terminalShowsCodexWaitingPrompt checks if the captured terminal output contains
// patterns indicating a Codex approval/permission prompt is visible.
func terminalShowsCodexWaitingPrompt(output string) bool {
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
		"would you like to run the following command",
		"do you want to approve network access",
		"would you like to grant these permissions",
		"would you like to apply this patch",
		"allow once",
		"allow for this session",
		"always allow",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// terminalShowsCodexIdlePrompt checks if the captured terminal output indicates
// the Codex agent has finished processing. The Codex TUI renders
// "esc to interrupt" only while the agent is actively working. Its absence,
// combined with the presence of any TUI content, indicates the agent is idle.
func terminalShowsCodexIdlePrompt(output string) bool {
	output = strings.ReplaceAll(output, "\u00a0", " ")
	lower := strings.ToLower(output)

	// Require some non-empty content to confirm the TUI is rendered.
	if strings.TrimSpace(lower) == "" {
		return false
	}

	// The "esc to interrupt" hint is rendered only while the agent is busy.
	return !strings.Contains(lower, "esc to interrupt")
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

func syncGeminiSession(ctx context.Context, queries *sqlc.Queries, homeDir string, sess sqlc.ListSessionsRow, tmuxRunner tmux.Runner, tmuxName string, state *monitorState) error {
	st := status.Status(sess.Status)
	sessionKey := sess.Name + ":" + sess.Path

	// File-based cancellation detection.
	if sess.AgentSessionID != "" {
		pattern := agent.GeminiSessionFilePattern(homeDir, sess.Path, sess.AgentSessionID)
		if pattern != "" {
			if cancelled, err := detectGeminiCancellation(ctx, queries, sess, pattern, st); err != nil {
				return err
			} else if cancelled {
				delete(state.capturePaneClearCount, sessionKey)
				delete(state.capturePromptSeen, sessionKey)
				return nil
			}
		}
	}

	// Capture-pane based waiting→running detection.
	if st != status.Waiting {
		delete(state.capturePaneClearCount, sessionKey)
		delete(state.capturePromptSeen, sessionKey)
		return nil
	}

	output, err := tmuxRunner.CapturePane(ctx, tmuxName)
	if err != nil {
		return nil // non-fatal
	}

	if terminalShowsGeminiWaitingPrompt(output) {
		state.capturePaneClearCount[sessionKey] = 0
		state.capturePromptSeen[sessionKey] = true
		return nil
	}

	if !state.capturePromptSeen[sessionKey] {
		return nil
	}

	state.capturePaneClearCount[sessionKey]++
	if state.capturePaneClearCount[sessionKey] < 2 {
		return nil // debounce: need 2 consecutive clears
	}

	delete(state.capturePaneClearCount, sessionKey)
	delete(state.capturePromptSeen, sessionKey)
	return queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
		Status:       string(status.Running),
		UpdatedAt:    timestamp.Now(),
		WaitingSince: "",
		Name:         sess.Name,
		Path:         sess.Path,
		Status_2:     string(st),
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
		Status:       string(status.Idle),
		UpdatedAt:    timestamp.Now(),
		WaitingSince: "",
		Name:         sess.Name,
		Path:         sess.Path,
		Status_2:     string(st),
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

type codexLogLine struct {
	Ts      string          `json:"ts"`
	Dir     string          `json:"dir"`
	Kind    string          `json:"kind"`
	Variant string          `json:"variant"`
	Payload json.RawMessage `json:"payload"`
}

// codexOpPayload is the internally-tagged payload of an op log line.
// Codex Op uses #[serde(tag = "type", rename_all = "snake_case")].
type codexOpPayload struct {
	Type     string `json:"type"`
	Decision string `json:"decision"`
}

func codexEventToStatus(line codexLogLine) (status.Status, bool) {
	switch line.Kind {
	case "session_end":
		return status.Idle, true
	case "app_event":
		// Codex TUI logs approval/permission requests as AppEvent::FullScreenApprovalRequest,
		// which appears as kind "app_event" with variant "FullScreenApprovalRequest".
		if line.Variant == "FullScreenApprovalRequest" {
			return status.Waiting, true
		}
	case "op":
		var op codexOpPayload
		if json.Unmarshal(line.Payload, &op) != nil {
			return "", false
		}
		switch op.Type {
		case "user_input", "user_turn",
			"user_input_answer", "request_user_input_response",
			"resolve_elicitation", "dynamic_tool_response",
			"request_permissions_response":
			return status.Running, true
		case "interrupt":
			return status.Idle, true
		case "exec_approval", "patch_approval":
			if op.Decision == "abort" {
				return status.Idle, true
			}
			return status.Running, true
		}
	}
	return "", false
}

// findLastCodexStatus reads the file backward in chunks and returns the most
// recent status-relevant Codex event. It stops as soon as a status event is
// found, avoiding a full file scan. Returns os.IsNotExist errors as-is.
func findLastCodexStatus(filePath string) (status.Status, string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return "", "", fmt.Errorf("seek end %q: %w", filePath, err)
	}
	if size == 0 {
		return "", "", nil
	}

	const chunkSize = 8192
	var remainder []byte
	offset := size

	for offset > 0 {
		readSize := min(int64(chunkSize), offset)
		offset -= readSize

		chunk := make([]byte, readSize)
		if _, err := f.ReadAt(chunk, offset); err != nil && err != io.EOF {
			return "", "", fmt.Errorf("read chunk %q: %w", filePath, err)
		}

		data := append(chunk, remainder...)
		parts := strings.Split(string(data), "\n")

		// First element may be a partial line unless we reached BOF.
		if offset > 0 {
			remainder = []byte(parts[0])
			parts = parts[1:]
		} else {
			remainder = nil
		}

		// Scan from newest (end) to oldest (start).
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] == "" {
				continue
			}
			var cl codexLogLine
			if json.Unmarshal([]byte(parts[i]), &cl) != nil {
				continue
			}
			if st, ok := codexEventToStatus(cl); ok {
				return st, cl.Ts, nil
			}
		}
	}

	// Process any remaining partial line from the very beginning.
	if len(remainder) > 0 {
		var cl codexLogLine
		if json.Unmarshal(remainder, &cl) == nil {
			if st, ok := codexEventToStatus(cl); ok {
				return st, cl.Ts, nil
			}
		}
	}

	return "", "", nil
}

func syncCodexSession(ctx context.Context, queries *sqlc.Queries, cacheDir string, sess sqlc.ListSessionsRow, tmuxRunner tmux.Runner, tmuxName string, state *monitorState) error {
	logPath := agent.CodexSessionLogPath(cacheDir, tmuxName)
	st := status.Status(sess.Status)
	sessionKey := sess.Name + ":" + sess.Path

	// JSONL-based detection: check for ops (user_turn, exec_approval, etc.)
	lastStatus, lastTs, err := findLastCodexStatus(logPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read codex session log %q: %w", logPath, err)
	}

	if lastStatus != "" && isAfter(lastTs, sess.UpdatedAt) && lastStatus != st {
		var waitingSince string
		if lastStatus == status.Waiting {
			waitingSince = timestamp.Now()
		}
		if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
			Status:       string(lastStatus),
			UpdatedAt:    timestamp.Now(),
			WaitingSince: waitingSince,
			Name:         sess.Name,
			Path:         sess.Path,
			Status_2:     sess.Status,
		}); err != nil {
			return fmt.Errorf("update codex status via log: %w", err)
		}
		delete(state.capturePaneClearCount, sessionKey)
		delete(state.capturePromptSeen, sessionKey)
		return nil
	}

	// Capture-pane detection: approval requests and turn completions are not
	// logged to the JSONL file, so use terminal content and file staleness.
	if st == status.Running {
		output, cpErr := tmuxRunner.CapturePane(ctx, tmuxName)
		if cpErr != nil {
			return nil
		}
		if terminalShowsCodexWaitingPrompt(output) {
			if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
				Status:       string(status.Waiting),
				UpdatedAt:    timestamp.Now(),
				WaitingSince: timestamp.Now(),
				Name:         sess.Name,
				Path:         sess.Path,
				Status_2:     string(st),
			}); err != nil {
				return fmt.Errorf("update codex status to waiting via capture-pane: %w", err)
			}
			state.capturePromptSeen[sessionKey] = true
			state.capturePaneClearCount[sessionKey] = 0
			return nil
		}

		// Idle detection: when the agent finishes a turn, the Codex TUI shows
		// the chat composer with its placeholder text. This text is only
		// rendered when the agent is not running.
		if terminalShowsCodexIdlePrompt(output) {
			state.capturePaneClearCount[sessionKey]++
			if state.capturePaneClearCount[sessionKey] >= 2 {
				delete(state.capturePaneClearCount, sessionKey)
				delete(state.capturePromptSeen, sessionKey)
				if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
					Status:       string(status.Idle),
					UpdatedAt:    timestamp.Now(),
					WaitingSince: "",
					Name:         sess.Name,
					Path:         sess.Path,
					Status_2:     string(st),
				}); err != nil {
					return fmt.Errorf("update codex status to idle via capture-pane: %w", err)
				}
			}
			return nil
		}
		state.capturePaneClearCount[sessionKey] = 0
	}

	// When waiting, check if the approval prompt has been dismissed.
	if st == status.Waiting {
		output, cpErr := tmuxRunner.CapturePane(ctx, tmuxName)
		if cpErr != nil {
			return nil
		}
		if terminalShowsCodexWaitingPrompt(output) {
			state.capturePaneClearCount[sessionKey] = 0
			state.capturePromptSeen[sessionKey] = true
			return nil
		}
		if !state.capturePromptSeen[sessionKey] {
			return nil
		}
		state.capturePaneClearCount[sessionKey]++
		if state.capturePaneClearCount[sessionKey] < 2 {
			return nil
		}
		delete(state.capturePaneClearCount, sessionKey)
		delete(state.capturePromptSeen, sessionKey)
		if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
			Status:       string(status.Running),
			UpdatedAt:    timestamp.Now(),
			WaitingSince: "",
			Name:         sess.Name,
			Path:         sess.Path,
			Status_2:     string(st),
		}); err != nil {
			return fmt.Errorf("update codex status to running via capture-pane: %w", err)
		}
	}

	return nil
}
