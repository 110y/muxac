CREATE TABLE IF NOT EXISTS sessions (
    name               TEXT NOT NULL,
    path               TEXT NOT NULL,
    status             TEXT NOT NULL,
    agent_session_id   TEXT NOT NULL DEFAULT '',
    agent_tool         TEXT NOT NULL DEFAULT '' CHECK (agent_tool IN ('', 'claude', 'codex', 'gemini')),
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    waiting_since      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (path, name)
);

CREATE TABLE IF NOT EXISTS monitor_heartbeat (
    id         INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    version    TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS debug_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    level      TEXT NOT NULL,
    message    TEXT NOT NULL,
    created_at TEXT NOT NULL
);
