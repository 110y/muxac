-- Disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- Drop "jsonl_entries" table
DROP TABLE `jsonl_entries`;
-- Add column "waiting_since" to table: "sessions"
ALTER TABLE `sessions` ADD COLUMN `waiting_since` text NOT NULL DEFAULT '';
-- Enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;
