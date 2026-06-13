# REPL runtime source map

Each line maps a contract taught in SKILL.md to the Go code that implements it.

- `server/cmd/multica/cmd_runtime_repl.go` registers `runtime repl`, `runtime next`, and `runtime result`, and installs this skill into `~/.claude/skills/multica-repl-runtime/`.
- `runtime repl` installs the skill, then runs the daemon in the foreground in repl executor mode via `startDaemonForeground(cmd, "repl")` (`server/cmd/multica/cmd_daemon.go`).
- `runtime next` GETs `http://127.0.0.1:<port>/runtime/next?wait=<seconds>` on the daemon loopback (port = `MULTICA_DAEMON_PORT` or `healthPortForProfile`) and prints `{"job": ...}`; the daemon holds the request open up to ~25s.
- `runtime result <job-id>` POSTs `{job_id, status, summary, error}` to `http://127.0.0.1:<port>/runtime/result` and reports whether it was delivered.
- `server/internal/daemon/health.go` serves `/runtime/next` (long-poll via `replBroker.Next`) and `/runtime/result` (`replBroker.Report`) on the same loopback listener as `/health` and `/repo/checkout`; both return 503 unless the daemon runs with `--executor repl`.
- `server/internal/daemon/broker.go` is the in-process broker: `Submit` (called by the repl backend) enqueues a job and blocks for a result; `Next` hands one job to one REPL session; `Report` routes the result back. Job ids are broker-generated (`job-<n>`), not Multica task ids, so a retried task cannot collide.
- `server/pkg/agent/repl.go` is the `repl` agent backend: instead of spawning `claude -p`, it calls `Config.ReplBroker.Submit` and emits periodic status beats so the daemon idle watchdog does not kill a task while a human works it.
- `server/internal/daemon/daemon.go` selects the `repl` backend (and wires `ReplBroker`) for every provider when `RuntimeExecutor == ExecutorRepl`; the daemon still registers, claims, heartbeats, checks out repos, builds the prompt, and completes/fails the task as usual.
- `server/internal/daemon/config.go` resolves `RuntimeExecutor` from `--executor` / `MULTICA_RUNTIME_EXECUTOR` (default `subprocess`).
