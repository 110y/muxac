package monitor

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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

// Run starts a monitoring loop that syncs session statuses between tmux and the database.
// It runs an initial sync immediately, then repeats every second.
// Returns nil on context cancellation.
func Run(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir, cacheDir string, logger *slog.Logger) error {
	if err := sync(ctx, tmuxRunner, queries, homeDir, cacheDir); err != nil {
		logger.ErrorContext(ctx, "sync failed", "error", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := sync(ctx, tmuxRunner, queries, homeDir, cacheDir); err != nil {
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

func sync(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir, cacheDir string) error {
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
		if err := syncSession(ctx, queries, homeDir, cacheDir, sess, alive); err != nil {
			errs = append(errs, fmt.Errorf("sync session %s/%s: %w", sess.Name, sess.Path, err))
		}
	}

	return errors.Join(errs...)
}

func syncSession(ctx context.Context, queries *sqlc.Queries, homeDir, cacheDir string, sess sqlc.ListSessionsRow, alive map[string]bool) error {
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
		return syncCodexSession(ctx, queries, cacheDir, sess, tmuxName)
	case agent.Claude:
		return syncClaudeCodeSession(ctx, queries, homeDir, sess)
	default:
		return nil
	}
}

func syncClaudeCodeSession(ctx context.Context, queries *sqlc.Queries, homeDir string, sess sqlc.ListSessionsRow) error {
	if sess.AgentSessionID == "" {
		return nil
	}

	jsonlPath := agent.JsonlPath(agent.Claude, homeDir, sess.Path, sess.AgentSessionID)
	if jsonlPath == "" {
		return nil
	}

	entryParams := sqlc.GetLatestJsonlEntryParams{
		SessionName: sess.Name,
		SessionPath: sess.Path,
	}

	prev, err := queries.GetLatestJsonlEntry(ctx, entryParams)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get latest jsonl entry: %w", err)
		}
		prev = sqlc.GetLatestJsonlEntryRow{}
	}

	f, err := os.Open(jsonlPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open jsonl %q: %w", jsonlPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var lastLine jsonlLine
	for scanner.Scan() {
		var line jsonlLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		lastLine = line
		if line.UUID == "" {
			continue
		}
		if err := queries.UpsertJsonlEntry(ctx, sqlc.UpsertJsonlEntryParams{
			SessionName: sess.Name,
			SessionPath: sess.Path,
			Uuid:        line.UUID,
			Timestamp:   line.Timestamp,
		}); err != nil {
			return fmt.Errorf("upsert jsonl entry: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan jsonl %q: %w", jsonlPath, err)
	}

	// Interruption check takes priority over waiting→running.
	if isInterruptionLine(lastLine) && isAfter(lastLine.Timestamp, sess.UpdatedAt) {
		st := status.Status(sess.Status)
		if st == status.Running || st == status.Waiting {
			if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
				Status:    string(status.Stopped),
				UpdatedAt: timestamp.Now(),
				Name:      sess.Name,
				Path:      sess.Path,
				Status_2:  string(st),
			}); err != nil {
				return fmt.Errorf("update status to stopped: %w", err)
			}
		}
		return nil
	}

	current, err := queries.GetLatestJsonlEntry(ctx, entryParams)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("get current jsonl entry: %w", err)
		}
		current = sqlc.GetLatestJsonlEntryRow{}
	}

	st := status.Status(sess.Status)
	if st == status.Waiting && current.Uuid != prev.Uuid && isAfter(current.Timestamp, sess.UpdatedAt) {
		if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
			Status:    string(status.Running),
			UpdatedAt: timestamp.Now(),
			Name:      sess.Name,
			Path:      sess.Path,
			Status_2:  string(st),
		}); err != nil {
			return fmt.Errorf("update status to running: %w", err)
		}
	}

	return nil
}

type codexLogLine struct {
	Ts      string          `json:"ts"`
	Dir     string          `json:"dir"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// codexEventMsg is the internally-tagged msg inside a codex_event payload.
// Codex uses #[serde(tag = "type", rename_all = "snake_case")].
type codexEventMsg struct {
	Type string `json:"type"`
}

// codexEventPayload is the payload of a codex_event log line.
// It wraps an Event struct with id + msg fields.
type codexEventPayload struct {
	Msg codexEventMsg `json:"msg"`
}

// codexOpPayload is the internally-tagged payload of an op log line.
type codexOpPayload struct {
	Type     string `json:"type"`
	Decision string `json:"decision"`
}

func codexEventToStatus(line codexLogLine) (status.Status, bool) {
	switch line.Kind {
	case "session_end":
		return status.Stopped, true
	case "codex_event":
		var p codexEventPayload
		if json.Unmarshal(line.Payload, &p) != nil {
			return "", false
		}
		switch p.Msg.Type {
		case "task_started", "turn_started":
			return status.Running, true
		case "task_complete", "turn_complete", "turn_aborted", "shutdown_complete":
			return status.Stopped, true
		case "exec_approval_request", "apply_patch_approval_request", "request_permissions", "request_user_input":
			return status.Waiting, true
		}
	case "op":
		var op codexOpPayload
		if json.Unmarshal(line.Payload, &op) != nil {
			return "", false
		}
		switch op.Type {
		case "user_input", "user_turn":
			return status.Running, true
		case "interrupt":
			return status.Stopped, true
		case "exec_approval", "patch_approval":
			if op.Decision == "abort" {
				return status.Stopped, true
			}
			return status.Running, true
		}
	}
	return "", false
}

func syncCodexSession(ctx context.Context, queries *sqlc.Queries, cacheDir string, sess sqlc.ListSessionsRow, tmuxName string) error {
	logPath := agent.CodexSessionLogPath(cacheDir, tmuxName)

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open codex session log %q: %w", logPath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var lastStatus status.Status
	var lastTs string
	for scanner.Scan() {
		var line codexLogLine
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			continue
		}
		if st, ok := codexEventToStatus(line); ok {
			lastStatus = st
			lastTs = line.Ts
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan codex session log %q: %w", logPath, err)
	}

	if lastStatus == "" {
		return nil
	}

	if !isAfter(lastTs, sess.UpdatedAt) {
		return nil
	}

	if lastStatus == status.Status(sess.Status) {
		return nil
	}

	return queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
		Status:    string(lastStatus),
		UpdatedAt: timestamp.Now(),
		Name:      sess.Name,
		Path:      sess.Path,
		Status_2:  sess.Status,
	})
}
