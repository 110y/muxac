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

// Run starts a monitoring loop that syncs session statuses between tmux and the database.
// It runs an initial sync immediately, then repeats every second.
// Returns nil on context cancellation.
func Run(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir string, logger *slog.Logger) error {
	if err := sync(ctx, tmuxRunner, queries, homeDir); err != nil {
		logger.ErrorContext(ctx, "sync failed", "error", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := sync(ctx, tmuxRunner, queries, homeDir); err != nil {
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

func sync(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir string) error {
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
		if err := syncSession(ctx, queries, homeDir, sess, alive); err != nil {
			errs = append(errs, fmt.Errorf("sync session %s/%s: %w", sess.Name, sess.Path, err))
		}
	}

	return errors.Join(errs...)
}

func syncSession(ctx context.Context, queries *sqlc.Queries, homeDir string, sess sqlc.ListSessionsRow, alive map[string]bool) error {
	tmuxName := pathkey.TmuxSessionName(sess.Name, sess.Path)

	if !alive[tmuxName] {
		if err := queries.DeleteSession(ctx, sqlc.DeleteSessionParams{
			Name: sess.Name,
			Path: sess.Path,
		}); err != nil {
			return fmt.Errorf("delete dead session: %w", err)
		}
		return nil
	}

	if sess.AgentSessionID == "" {
		return nil
	}

	tool := agent.ToolFromString(sess.AgentTool)
	jsonlPath := agent.JsonlPath(tool, homeDir, sess.Path, sess.AgentSessionID)
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
		return nil
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

	// Interruption check takes priority over waiting→running.
	if isInterruptionLine(lastLine) && lastLine.Timestamp > sess.UpdatedAt {
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
	if st == status.Waiting && current.Uuid != prev.Uuid && current.Timestamp > sess.UpdatedAt {
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
