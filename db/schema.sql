CREATE TABLE IF NOT EXISTS sessions (
    name               TEXT NOT NULL,
    path               TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('running', 'waiting', 'stopped', 'unknown')),
    agent_session_id   TEXT NOT NULL DEFAULT '',
    agent_tool         TEXT NOT NULL DEFAULT '' CHECK (agent_tool IN ('', 'claude')),
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (path, name)
);

CREATE TABLE IF NOT EXISTS jsonl_entries (
    session_name TEXT NOT NULL,
    session_path TEXT NOT NULL,
    uuid         TEXT NOT NULL,
    timestamp    TEXT NOT NULL,
    PRIMARY KEY (session_name, session_path, uuid),
    FOREIGN KEY (session_name, session_path) REFERENCES sessions(name, path) ON DELETE CASCADE
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
