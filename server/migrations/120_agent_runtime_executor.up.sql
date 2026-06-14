-- How a local daemon executes claimed tasks for this runtime:
--   'subprocess' (default) — the daemon spawns the provider CLI per task
--   'repl'                  — tasks are handed to a human-launched Claude Code
--                             REPL session via the local broker
-- This is orthogonal to runtime_mode (local/cloud): it distinguishes a
-- headless local daemon from a REPL-driven one so the runtime list can show
-- which mode a machine is currently running. It is a per-daemon attribute
-- (one daemon runs one executor), so the existing
-- (workspace_id, daemon_id, provider) uniqueness is untouched — the upsert
-- simply refreshes this column to the daemon's current mode.
ALTER TABLE agent_runtime
    ADD COLUMN executor TEXT NOT NULL DEFAULT 'subprocess'
    CHECK (executor IN ('subprocess', 'repl'));
