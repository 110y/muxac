package hook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/pathkey"
	"github.com/110y/muxac/internal/status"
	"github.com/110y/muxac/internal/timestamp"
	"github.com/110y/muxac/internal/tmux"
)

type hookInput struct {
	HookEventName    string `json:"hook_event_name"`
	SessionID        string `json:"session_id"`
	NotificationType string `json:"notification_type"`
}

// Run reads a hook event from r and upserts the corresponding session status in the database.
// sessionName comes from the MUXAC_SESSION_NAME env var, projectDir from the detected tool's project dir.
//
// Claude Code's events are ignored: it reports its status through the Program
// Status Protocol (OSC 7501), which `muxac wrap` reads from its terminal.
func Run(ctx context.Context, r io.Reader, tmuxRunner tmux.Runner, queries *sqlc.Queries, sessionName, projectDir string, tool agent.Tool) error {
	if sessionName == "" || tool == agent.Claude {
		return nil
	}

	var input hookInput
	if err := json.NewDecoder(r).Decode(&input); err != nil {
		return err
	}

	// Gemini CLI reports a permission request as a ToolPermission notification.
	eventName := input.HookEventName
	if eventName == "Notification" && input.NotificationType == "ToolPermission" {
		eventName = "PermissionRequest"
	}

	event := agent.NormalizeEvent(tool, eventName)

	target, ok := status.FromEvent(event)
	if !ok {
		return nil
	}

	// Only record events for sessions muxac actually manages. MUXAC_SESSION_NAME
	// is inherited by every child process of the tmux session, so a nested agent
	// launched in a subdirectory (e.g. a sub-session started inside the pane)
	// would otherwise create a phantom session row keyed on its own cwd, even
	// though no muxac tmux session exists for that directory.
	if !tmuxRunner.HasSession(ctx, pathkey.TmuxSessionName(sessionName, projectDir)) {
		return nil
	}

	// Once the agent reports its status through the Program Status Protocol
	// (OSC 7501), `muxac wrap` keeps the session status in sync with those
	// reports, which cover every transition — including the ones no hook fires
	// for, such as a permission prompt dismissed with Escape. Hook events would
	// only race with them, so they are ignored for such a session.
	programStatus, err := queries.GetSessionProgramStatus(ctx, sqlc.GetSessionProgramStatusParams{
		Name: sessionName,
		Path: projectDir,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if programStatus != 0 {
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

	if tool != agent.Gemini && !status.IsValidTransition(current, event) {
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
		}
	}

	return nil
}
