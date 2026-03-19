-- Create "new_sessions" table without CHECK constraint on status
CREATE TABLE `new_sessions` (
  `name` text NOT NULL,
  `path` text NOT NULL,
  `status` text NOT NULL,
  `agent_session_id` text NOT NULL DEFAULT '',
  `agent_tool` text NOT NULL DEFAULT '',
  `created_at` text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  `updated_at` text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  PRIMARY KEY (`name`, `path`),
  CHECK (agent_tool IN ('', 'claude', 'codex', 'gemini'))
);
-- Copy rows from old table "sessions" to new temporary table "new_sessions"
INSERT INTO `new_sessions` (`name`, `path`, `status`, `agent_session_id`, `agent_tool`, `created_at`, `updated_at`) SELECT `name`, `path`, `status`, `agent_session_id`, `agent_tool`, `created_at`, `updated_at` FROM `sessions`;
-- Drop "sessions" table after copying rows
DROP TABLE `sessions`;
-- Rename temporary table "new_sessions" to "sessions"
ALTER TABLE `new_sessions` RENAME TO `sessions`;
-- Rename 'stopped' status to 'idle' in the new table (no CHECK constraint)
UPDATE `sessions` SET `status` = 'idle' WHERE `status` = 'stopped';
