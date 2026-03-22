-- name: UpsertSessionStatus :exec
INSERT INTO sessions (name, path, status, updated_at, waiting_since)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (name, path) DO UPDATE SET
    status = excluded.status,
    updated_at = excluded.updated_at,
    waiting_since = excluded.waiting_since;

-- name: GetSessionStatus :one
SELECT status FROM sessions WHERE name = ? AND path = ?;

-- name: ListSessions :many
SELECT name, path, status, agent_session_id, agent_tool, updated_at, waiting_since FROM sessions;

-- name: UpdateSessionStatusIfUnchanged :exec
UPDATE sessions SET status = ?, updated_at = ?, waiting_since = ''
WHERE name = ? AND path = ? AND status = ?;

-- name: UpdateAgentSessionID :exec
UPDATE sessions SET agent_session_id = ?, updated_at = ?
WHERE name = ? AND path = ?;

-- name: UpdateAgentTool :exec
UPDATE sessions SET agent_tool = ?, updated_at = ?
WHERE name = ? AND path = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE name = ? AND path = ?;

-- name: UpsertMonitorHeartbeat :exec
INSERT INTO monitor_heartbeat (id, version, updated_at)
VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    version = excluded.version,
    updated_at = excluded.updated_at;

-- name: GetMonitorHeartbeat :one
SELECT version, updated_at FROM monitor_heartbeat WHERE id = 1;

-- name: InsertDebugLog :exec
INSERT INTO debug_log (level, message, created_at) VALUES (?, ?, ?);

-- name: DeleteOldDebugLog :exec
DELETE FROM debug_log WHERE created_at < ?;
