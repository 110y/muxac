package hook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/status"
	"github.com/110y/muxac/internal/timestamp"
)

type hookInput struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
}

// Run reads a hook event from r and upserts the corresponding session status in the database.
// sessionName comes from the MUXAC_SESSION_NAME env var, projectDir from the detected tool's project dir.
func Run(ctx context.Context, r io.Reader, queries *sqlc.Queries, sessionName, projectDir string, tool agent.Tool) error {
	if sessionName == "" {
		return nil
	}

	var input hookInput
	if err := json.NewDecoder(r).Decode(&input); err != nil {
		return err
	}

	event := agent.NormalizeEvent(tool, input.HookEventName)

	target, ok := status.FromEvent(event)
	if !ok {
		return nil
	}

	currentStr, err := queries.GetSessionStatus(ctx, sqlc.GetSessionStatusParams{
		Name: sessionName,
		Path: projectDir,
	})

	var current status.Status
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			current = status.Unknown
		} else {
			return err
		}
	} else {
		current = status.Status(currentStr)
	}

	if !status.IsValidTransition(current, event) {
		return nil
	}

	if target != current {
		if err := queries.UpsertSessionStatus(ctx, sqlc.UpsertSessionStatusParams{
			Name:      sessionName,
			Path:      projectDir,
			Status:    string(target),
			UpdatedAt: timestamp.Now(),
		}); err != nil {
			return err
		}
	}

	if event == "SessionStart" {
		if err := queries.UpdateAgentTool(ctx, sqlc.UpdateAgentToolParams{
			AgentTool: tool.String(),
			UpdatedAt: timestamp.Now(),
			Name:      sessionName,
			Path:      projectDir,
		}); err != nil {
			return err
		}

		if input.SessionID != "" {
			if err := queries.UpdateAgentSessionID(ctx, sqlc.UpdateAgentSessionIDParams{
				AgentSessionID: input.SessionID,
				UpdatedAt:      timestamp.Now(),
				Name:           sessionName,
				Path:           projectDir,
			}); err != nil {
				return err
			}
			if err := queries.DeleteJsonlEntries(ctx, sqlc.DeleteJsonlEntriesParams{
				SessionName: sessionName,
				SessionPath: projectDir,
			}); err != nil {
				return err
			}
		}
	}

	return nil
}
