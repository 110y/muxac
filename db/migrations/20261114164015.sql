-- Disable the enforcement of foreign-keys constraints
PRAGMA foreign_keys = off;
-- Drop "monitor_heartbeat" table
DROP TABLE `monitor_heartbeat`;
-- Enable back the enforcement of foreign-keys constraints
PRAGMA foreign_keys = on;
