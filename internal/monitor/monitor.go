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
	"regexp"
	"strings"
	"time"

	"github.com/110y/muxac/internal/agent"
	"github.com/110y/muxac/internal/database/sqlc"
	"github.com/110y/muxac/internal/pathkey"
	"github.com/110y/muxac/internal/status"
	"github.com/110y/muxac/internal/timestamp"
	"github.com/110y/muxac/internal/tmux"
)

// claudeActiveQuietPeriod is how long a Claude Code session must have written no
// conversation turn (assistant message or tool result) before the terminal alone
// is trusted to decide a running session has gone idle. While the agent works it
// appends those lines to the JSONL, so a recent conversation write means the
// session is busy even if the on-screen spinner is momentarily not where we look
// for it (e.g. scrolled out of view by a multi-line draft in the input box). This
// keeps the capture-pane idle detection from flapping mid-turn.
//
// Crucially it ignores the bookkeeping Claude Code writes at submit time (the
// prompt echo, "ai-title" and "mode" markers): those are not conversation turns,
// so an interrupt right after starting — which produces no conversation output —
// is detected promptly instead of waiting those writes out.
const claudeActiveQuietPeriod = 5 * time.Second

// captureRunningIdleThreshold is how many consecutive sync ticks a running
// session must show no terminal activity before the capture-pane fallback flips
// it to idle. A genuinely busy agent regularly shows no spinner for a beat
// between steps (a completed tool result and its next message, model-invocation
// latency), and for sessions whose transcript muxac cannot usefully read — e.g. a
// freshly restarted session whose file holds only an "ai-title" line — the
// conversationRecentlyWritten guard never fires, leaving the terminal as the only
// signal. A short debounce then flickers such a session running↔idle on every
// brief gap. Requiring a sustained idle window absorbs those gaps; the genuine
// case this fallback exists for (Escape right after submit, no Stop hook) is a
// lasting idle, so it is still caught, just a few seconds later.
const captureRunningIdleThreshold = 8

// captureWaitingPromptThreshold is how many consecutive sync ticks a permission
// prompt must be visible before an idle or running session is flipped to waiting
// (see syncClaudeCodeSession). A small debounce ignores a transient match while
// still surfacing a real, stable prompt promptly.
const captureWaitingPromptThreshold = 2

// waitingIdleClearThreshold is how many consecutive sync ticks a dismissed
// permission prompt must show no agent activity — neither a processing status
// line nor a freshly written conversation turn — before a waiting session is
// moved to idle. It debounces transient blank frames and, crucially, gives a
// just-approved session time to start rendering its work (the brief gap between
// the prompt closing and the next "Running…"/tool output, e.g. while a quick
// approved edit is followed by model-invocation latency). Without that margin an
// approved, working session can be momentarily mislabeled idle.
const waitingIdleClearThreshold = 3

// conversationRecentlyWritten reports whether maxConvTimestamp — the timestamp of
// the most recent user/assistant transcript line — is within d of now. An empty
// or unparseable timestamp counts as not recent.
func conversationRecentlyWritten(maxConvTimestamp string, d time.Duration) bool {
	if maxConvTimestamp == "" {
		return false
	}
	t, err := parseTimestamp(maxConvTimestamp)
	if err != nil {
		return false
	}
	return time.Since(t) < d
}

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
	// captureRunningClearCount debounces the running→idle transition that is
	// detected purely from the terminal (see syncClaudeCodeSession).
	captureRunningClearCount map[string]int
	// captureRunningWaitingCount debounces the running→waiting transition that is
	// detected purely from the terminal (a permission prompt appearing on a
	// session the hooks left marked running; see syncClaudeCodeSession).
	captureRunningWaitingCount map[string]int
}

// Run starts a monitoring loop that syncs session statuses between tmux and the database.
// It runs an initial sync immediately, then repeats every second.
// Returns nil on context cancellation.
func Run(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir string, logger *slog.Logger) error {
	state := &monitorState{
		capturePaneClearCount:      make(map[string]int),
		capturePromptSeen:          make(map[string]bool),
		captureRunningClearCount:   make(map[string]int),
		captureRunningWaitingCount: make(map[string]int),
	}

	if err := sync(ctx, tmuxRunner, queries, homeDir, state); err != nil {
		logger.ErrorContext(ctx, "sync failed", "error", err)
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := sync(ctx, tmuxRunner, queries, homeDir, state); err != nil {
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
	UUID              string       `json:"uuid"`
	Timestamp         string       `json:"timestamp"`
	Type              string       `json:"type"`
	Message           jsonlMessage `json:"message"`
	IsApiErrorMessage bool         `json:"isApiErrorMessage"`
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

// isAPIErrorLine reports whether the line is the synthetic assistant message
// Claude Code writes after API retries are exhausted (e.g. 529 Overloaded).
// No Stop hook fires in this case, so monitoring must detect it via the JSONL.
func isAPIErrorLine(line jsonlLine) bool {
	return line.Type == "assistant" && line.IsApiErrorMessage
}

// isConversationLine reports whether the line is an actual conversation turn
// (a user or assistant message). Claude Code also appends non-conversational
// bookkeeping lines — such as "last-prompt", "file-history-snapshot",
// "ai-title", "permission-mode" or "mode" — after real events, including
// immediately after an interruption.
//
// Interruption and API-error detection must therefore inspect the last
// conversation line rather than the literal last line of the JSONL. Otherwise a
// trailing bookkeeping line (e.g. the "last-prompt" marker written when the user
// stops the agent with Escape right after starting) would mask the interruption
// and leave the session stuck in "running".
func isConversationLine(line jsonlLine) bool {
	return line.Type == "user" || line.Type == "assistant"
}

// claudeTokenReadoutPattern matches the "↑/↓ N tokens" usage readout on Claude
// Code's live status line, e.g. the "↓ 41.0k tokens" in
// "✢ Running… (9m 45s · ↓ 41.0k tokens)". The leading ↑/↓ arrow makes it
// unambiguous: it is rendered only on the live status line and never in the
// scrollback (a finished turn reads "Worked for 7m 31s" / "Brewed for 35m" with
// no readout, and the "/clear to save N tokens" hint carries no arrow). It is
// therefore matched across the whole pane — which is what lets a running turn be
// detected even when a multi-line composer draft scrolls the status line above
// the tail window inspected for the spinner (see claudeShowsActivity).
var claudeTokenReadoutPattern = regexp.MustCompile(`[↑↓]\s*\d[\d.,]*\s*[km]?\s*tokens`)

// claudeSpinnerPattern matches the spinner word's trailing "…" (U+2026) ellipsis,
// e.g. the "g…" in "Running…" or "Cogitating…". It is present even before the
// token readout appears (right after a turn starts). Matching a letter
// immediately followed by "…" ignores an ellipsis used elsewhere (the
// "… +15 lines" fold indicator or "// …" in shown code, where a space precedes
// it); a finished "Worked for 7m 31s" has no ellipsis. Unlike the token readout
// this also occurs in scrollback — a completed "Reading 1 file…" / "Listing 1
// directory…" tool line ends the same way — so it is only trusted in the tail
// just above the input box.
var claudeSpinnerPattern = regexp.MustCompile(`[a-z]…`)

// claudeShowsActivity reports whether the captured terminal indicates the Claude
// Code agent is still busy. Empty output is treated as busy because the TUI may
// briefly render nothing during transitions.
func claudeShowsActivity(output string) bool {
	output = strings.ReplaceAll(output, " ", " ")

	lower := strings.ToLower(output)

	// Signals rendered only on the live status line, never in the scrollback, are
	// matched across the whole pane so content below the status line — a
	// multi-line draft in the composer, or an @-mention / slash-command menu —
	// cannot scroll them out of the tail window and make a busy session look idle:
	//   - "esc to interrupt": the interrupt hint (absent in some versions).
	//   - the "↑/↓ N tokens" usage readout: present once the turn produces tokens.
	if strings.Contains(lower, "esc to interrupt") || claudeTokenReadoutPattern.MatchString(lower) {
		return true
	}

	// The spinner ellipsis can also appear in the scrollback (e.g. a completed
	// "Reading 1 file…" tool line), so it is only trusted in the tail just above
	// the input box.
	var nonEmpty []string
	for l := range strings.SplitSeq(output, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			nonEmpty = append(nonEmpty, t)
		}
	}
	if len(nonEmpty) == 0 {
		return true
	}
	if len(nonEmpty) > 8 {
		nonEmpty = nonEmpty[len(nonEmpty)-8:]
	}

	return claudeSpinnerPattern.MatchString(strings.ToLower(strings.Join(nonEmpty, "\n")))
}

func sync(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir string, state *monitorState) error {
	var errs []error

	threshold := time.Now().Add(-7 * 24 * time.Hour).UTC().Format(timestamp.Format)
	if err := queries.DeleteOldDebugLog(ctx, threshold); err != nil {
		errs = append(errs, fmt.Errorf("delete old debug log: %w", err))
	}

	if err := queries.UpsertMonitorHeartbeat(ctx, sqlc.UpsertMonitorHeartbeatParams{
		Version:   monitorBuildID,
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
		if err := syncSession(ctx, tmuxRunner, queries, homeDir, sess, alive, state); err != nil {
			errs = append(errs, fmt.Errorf("sync session %s/%s: %w", sess.Name, sess.Path, err))
		}
	}

	return errors.Join(errs...)
}

func syncSession(ctx context.Context, tmuxRunner tmux.Runner, queries *sqlc.Queries, homeDir string, sess sqlc.ListSessionsRow, alive map[string]bool, state *monitorState) error {
	tmuxName := pathkey.TmuxSessionName(sess.Name, sess.Path)

	if !alive[tmuxName] {
		if err := queries.DeleteSession(ctx, sqlc.DeleteSessionParams{
			Name: sess.Name,
			Path: sess.Path,
		}); err != nil {
			return fmt.Errorf("delete dead session: %w", err)
		}
		delete(state.capturePaneClearCount, sess.Name+":"+sess.Path)
		delete(state.capturePromptSeen, sess.Name+":"+sess.Path)
		delete(state.captureRunningClearCount, sess.Name+":"+sess.Path)
		delete(state.captureRunningWaitingCount, sess.Name+":"+sess.Path)
		return nil
	}

	switch agent.ToolFromString(sess.AgentTool) {
	case agent.Codex:
		return syncCodexSession(ctx, queries, sess, tmuxRunner, tmuxName, state)
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

	// Timestamp of the most recent user/assistant line in the transcript tail
	// (bookkeeping markers and attachments excluded); used to tell active work
	// apart from an idle-looking terminal in the running→idle check below.
	var maxConvTimestamp string

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
				// Track the last conversation turn only: Claude Code appends
				// bookkeeping marker lines after an interruption, and treating
				// those as the latest line would mask the interruption.
				if isConversationLine(line) {
					lastLine = line
					if line.Timestamp != "" && (maxConvTimestamp == "" || isAfter(line.Timestamp, maxConvTimestamp)) {
						maxConvTimestamp = line.Timestamp
					}
				}
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

			// API error termination (e.g. 529 Overloaded after retry exhaustion):
			// Claude Code does not fire a Stop hook in this case, so the session
			// would otherwise remain stuck in `running`.
			if isAPIErrorLine(lastLine) && isAfter(lastLine.Timestamp, sess.UpdatedAt) {
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
						return fmt.Errorf("update status to idle on api error: %w", err)
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

	st := status.Status(sess.Status)
	sessionKey := sess.Name + ":" + sess.Path

	if st == status.Running {
		delete(state.capturePaneClearCount, sessionKey)
		delete(state.capturePromptSeen, sessionKey)

		output, err := tmuxRunner.CapturePane(ctx, tmuxName)
		if err != nil {
			return nil // non-fatal
		}

		// Running → waiting via capture-pane: a permission prompt is on screen but
		// the session is still marked running because no hook moved it to waiting
		// (the permission Notification can be missed, arrive out of order, or use a
		// notification_type muxac does not recognise). This is checked before the
		// conversation-activity guard below, because the tool call that triggers the
		// prompt is itself a fresh conversation turn and would otherwise keep the
		// session "running". Debounced so a transient match does not flip it.
		if terminalShowsPermissionPrompt(output) {
			delete(state.captureRunningClearCount, sessionKey)
			state.captureRunningWaitingCount[sessionKey]++
			if state.captureRunningWaitingCount[sessionKey] < captureWaitingPromptThreshold {
				return nil // debounce: need a stable prompt, not a transient match
			}
			delete(state.captureRunningWaitingCount, sessionKey)
			if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
				Status:       string(status.Waiting),
				UpdatedAt:    timestamp.Now(),
				WaitingSince: timestamp.Now(),
				Name:         sess.Name,
				Path:         sess.Path,
				Status_2:     string(st),
			}); err != nil {
				return fmt.Errorf("update claude status to waiting via capture-pane: %w", err)
			}
			return nil
		}
		delete(state.captureRunningWaitingCount, sessionKey)

		// The agent is still working if it recently wrote a conversation turn.
		// This is independent of terminal layout, so it stops the status from
		// flapping to idle during brief on-screen lulls between steps — while
		// still letting an interrupt be detected promptly, since submit-time
		// bookkeeping is not a conversation turn.
		if conversationRecentlyWritten(maxConvTimestamp, claudeActiveQuietPeriod) {
			delete(state.captureRunningClearCount, sessionKey)
			return nil
		}

		// Still busy: leave the status as running and reset the idle debounce.
		if claudeShowsActivity(output) {
			delete(state.captureRunningClearCount, sessionKey)
			return nil
		}

		// Running → idle via capture-pane: catch interruptions that leave no trace
		// in the JSONL and fire no Stop hook — e.g. pressing Escape immediately
		// after submitting a prompt, before the agent emits any output. In that
		// case Claude Code writes neither a "[Request interrupted by user]" line
		// nor a Stop event, so the session would otherwise stay stuck in "running".
		// The only signal is the terminal, which shows the processing status line
		// solely while the agent is busy.
		state.captureRunningClearCount[sessionKey]++
		if state.captureRunningClearCount[sessionKey] < captureRunningIdleThreshold {
			return nil // debounce: need a sustained idle window, not a brief gap
		}
		delete(state.captureRunningClearCount, sessionKey)
		if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
			Status:       string(status.Idle),
			UpdatedAt:    timestamp.Now(),
			WaitingSince: "",
			Name:         sess.Name,
			Path:         sess.Path,
			Status_2:     string(st),
		}); err != nil {
			return fmt.Errorf("update claude status to idle via capture-pane: %w", err)
		}
		return nil
	}

	// Capture-pane based detection: when status is waiting and JSONL shows no new entries,
	// check the terminal content for approval prompt visibility.
	if st != status.Waiting {
		delete(state.capturePaneClearCount, sessionKey)
		delete(state.capturePromptSeen, sessionKey)
		delete(state.captureRunningClearCount, sessionKey)

		// Idle → waiting via capture-pane: a permission prompt appeared on a
		// session the hooks left marked idle (a missed or unmapped permission
		// notification, or a status that lagged the agent). Surface it as waiting
		// so it is shown as needing the user, rather than going idle → running →
		// waiting once a later hook fires. Running is handled in the block above;
		// other states have no prompt to detect.
		if st == status.Idle {
			output, err := tmuxRunner.CapturePane(ctx, tmuxName)
			if err == nil && terminalShowsPermissionPrompt(output) {
				state.captureRunningWaitingCount[sessionKey]++
				if state.captureRunningWaitingCount[sessionKey] < captureWaitingPromptThreshold {
					return nil // debounce: need a stable prompt, not a transient match
				}
				delete(state.captureRunningWaitingCount, sessionKey)
				if err := queries.UpdateSessionStatusIfUnchanged(ctx, sqlc.UpdateSessionStatusIfUnchangedParams{
					Status:       string(status.Waiting),
					UpdatedAt:    timestamp.Now(),
					WaitingSince: timestamp.Now(),
					Name:         sess.Name,
					Path:         sess.Path,
					Status_2:     string(st),
				}); err != nil {
					return fmt.Errorf("update claude status to waiting via capture-pane: %w", err)
				}
				return nil
			}
		}
		delete(state.captureRunningWaitingCount, sessionKey)
		return nil
	}
	delete(state.captureRunningClearCount, sessionKey)
	delete(state.captureRunningWaitingCount, sessionKey)
	output, err := tmuxRunner.CapturePane(ctx, tmuxName)
	if err != nil {
		return nil // non-fatal
	}

	if terminalShowsWaitingPrompt(output) {
		state.capturePaneClearCount[sessionKey] = 0
		state.capturePromptSeen[sessionKey] = true
		return nil
	}

	// Only act on "prompt gone" if the prompt was previously seen. This prevents
	// false transitions when the prompt hasn't rendered yet or uses text not
	// matching the known patterns.
	if !state.capturePromptSeen[sessionKey] {
		return nil
	}

	// The prompt was dismissed; how it was dismissed decides the next state.
	//
	// Approved: the agent resumes, so either the terminal shows its processing
	// status line or the transcript gains a fresh conversation turn (e.g. the
	// tool_result of a just-approved tool). Switch to running the moment either
	// positive signal appears — without waiting out the idle debounce — so an
	// approved, working session is never briefly shown idle while it spins back up
	// (the gap between the prompt closing and the next "Running…"/tool output).
	if claudeShowsActivity(output) || conversationRecentlyWritten(maxConvTimestamp, claudeActiveQuietPeriod) {
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
			return fmt.Errorf("update status to running via capture-pane: %w", err)
		}
		return nil
	}

	// Cancelled (e.g. Escape at the prompt): the agent stops and shows no
	// activity. Claude Code fires no usable Stop hook here (Waiting+Stop is an
	// invalid transition); the "[Request interrupted by user for tool use]" line is
	// handled above and, while it lags, the session stays waiting and is checked
	// again each tick. Sustained absence of any activity is the fallback signal:
	// requiring several consecutive idle observations debounces transient blank
	// frames and gives a just-approved session time to begin rendering, yielding a
	// direct waiting → idle (never waiting → running → idle) on cancellation.
	state.capturePaneClearCount[sessionKey]++
	if state.capturePaneClearCount[sessionKey] < waitingIdleClearThreshold {
		return nil // debounce: need sustained absence of activity
	}
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
		return fmt.Errorf("update status to idle via capture-pane: %w", err)
	}
	return nil
}

// permissionPromptMarkers are substrings that appear only in a Claude Code
// permission/selection prompt's chrome — its footer hint line and its option
// list — and not in ordinary assistant prose. The "esc to cancel" footer
// accompanies the Bash/tool, plan-approval and question prompts.
var permissionPromptMarkers = []string{
	"esc to cancel",
	"yes, allow once",
	"yes, allow always",
	"no, and tell claude what to do differently",
	"do you trust the files",
	"use arrow keys to navigate",
	"tab to amend",
	"ctrl+e to explain",
}

// terminalShowsPermissionPrompt reports whether the captured terminal shows a
// permission/selection prompt, using only the prompt-chrome markers above. Unlike
// terminalShowsWaitingPrompt — which also matches looser, prose-like phrases ("do
// you want", "would you like") and is used to keep an already-waiting session
// waiting — this is safe to drive a status change *into* waiting from idle or
// running: an agent that merely ends a turn with "Would you like me to…?" must
// not be mistaken for one blocked on a prompt.
func terminalShowsPermissionPrompt(output string) bool {
	output = strings.ReplaceAll(output, " ", " ")

	allLines := strings.Split(output, "\n")
	var nonEmpty []string
	for _, l := range allLines {
		if trimmed := strings.TrimSpace(l); trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	if len(nonEmpty) > 15 {
		nonEmpty = nonEmpty[len(nonEmpty)-15:]
	}
	lower := strings.ToLower(strings.Join(nonEmpty, "\n"))

	for _, p := range permissionPromptMarkers {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
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

// syncCodexSession detects user interruptions (Ctrl+C) that Codex does not
// signal via its hook system. Without this, a `running` or `waiting` session
// would remain stuck because no Stop hook fires when the agent is interrupted.
//
// It uses capture-pane heuristics: while the agent is busy, the Codex TUI
// renders "esc to interrupt"; while waiting on approval, the permission
// prompt keywords are visible. If neither indicator is present for two
// consecutive sync ticks, the session is transitioned to idle.
func syncCodexSession(ctx context.Context, queries *sqlc.Queries, sess sqlc.ListSessionsRow, tmuxRunner tmux.Runner, tmuxName string, state *monitorState) error {
	st := status.Status(sess.Status)
	sessionKey := sess.Name + ":" + sess.Path

	if st != status.Running && st != status.Waiting {
		delete(state.capturePaneClearCount, sessionKey)
		return nil
	}

	output, err := tmuxRunner.CapturePane(ctx, tmuxName)
	if err != nil {
		return nil
	}

	if codexShowsActivity(output) {
		state.capturePaneClearCount[sessionKey] = 0
		return nil
	}

	state.capturePaneClearCount[sessionKey]++
	if state.capturePaneClearCount[sessionKey] < 2 {
		return nil
	}
	delete(state.capturePaneClearCount, sessionKey)

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
