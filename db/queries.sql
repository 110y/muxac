-- name: UpsertSessionStatus :exec
INSERT INTO sessions (name, path, status, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (name, path) DO UPDATE SET
    status = excluded.status,
    updated_at = excluded.updated_at;

-- name: GetSessionStatus :one
SELECT status FROM sessions WHERE name = ? AND path = ?;

-- name: GetSessionProgramStatus :one
SELECT program_status FROM sessions WHERE name = ? AND path = ?;

-- name: ListSessions :many
SELECT name, path, status, agent_session_id, agent_tool, updated_at, program_status FROM sessions;

-- name: GetSession :one
SELECT name, path, status, agent_session_id, agent_tool, updated_at, program_status FROM sessions
WHERE name = ? AND path = ?;

-- name: InitSessionStatus :exec
INSERT INTO sessions (name, path, status, updated_at, program_status)
VALUES (?, ?, ?, ?, 0)
ON CONFLICT (name, path) DO UPDATE SET
    status = excluded.status,
    updated_at = excluded.updated_at,
    program_status = excluded.program_status;

-- name: EnsureSession :exec
INSERT INTO sessions (name, path, status, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (name, path) DO NOTHING;

-- name: UpdateSessionProgramStatus :exec
UPDATE sessions SET status = ?, updated_at = ?, program_status = 1
WHERE name = ? AND path = ?;

-- name: UpdateSessionStatusIfUnchanged :exec
UPDATE sessions SET status = ?, updated_at = ?
WHERE name = ? AND path = ? AND status = ?;

-- name: UpdateAgentSessionID :exec
UPDATE sessions SET agent_session_id = ?, updated_at = ?
WHERE name = ? AND path = ?;

-- name: UpdateAgentTool :exec
UPDATE sessions SET agent_tool = ?, updated_at = ?
WHERE name = ? AND path = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE name = ? AND path = ?;

-- name: InsertDebugLog :exec
INSERT INTO debug_log (level, message, created_at) VALUES (?, ?, ?);

-- name: DeleteOldDebugLog :exec
DELETE FROM debug_log WHERE created_at < ?;
