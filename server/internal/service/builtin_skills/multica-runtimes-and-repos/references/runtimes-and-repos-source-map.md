# Runtimes and repos source map

- `server/cmd/multica/cmd_runtime.go` registers `runtime list`, `usage`, `activity`, and `update`.
- `runtime list` reads `/api/runtimes` and prints `id`, `name`, `runtime_mode`, `provider`, `status`, and `last_seen_at`.
- `runtime update` posts to `/api/runtimes/{runtime-id}/update`; with `--wait` it polls update status.
- `server/cmd/multica/cmd_runtime_repl.go` registers `runtime repl`, `runtime next`, `runtime result`, and `runtime renew`, and installs the embedded `multica-repl-runtime` skill into `~/.claude/skills/`.
- `runtime repl` calls `startDaemonForeground(cmd, "repl")` (`server/cmd/multica/cmd_daemon.go`); `--executor` / `MULTICA_RUNTIME_EXECUTOR` resolves `Config.RuntimeExecutor` in `server/internal/daemon/config.go` (`ExecutorSubprocess` default, `ExecutorRepl`).
- `runtime next` GETs, `runtime result`/`runtime renew` POST the daemon loopback `/runtime/next`, `/runtime/result`, `/runtime/renew`, served in `server/internal/daemon/health.go` against the broker in `server/internal/daemon/broker.go`; all 503 unless `RuntimeExecutor == ExecutorRepl`.
- Broker lease/visibility timeout: `Next` starts a lease on the delivered job; `runReaper` (`server/internal/daemon/daemon.go` launches it) re-enqueues jobs whose lease elapses with no `Report`/`Renew`, recovering a dead session's task. Lease default `DefaultRuntimeReplLease` (30m) via `--repl-lease` / `MULTICA_RUNTIME_REPL_LEASE`.
- In repl mode, `server/internal/daemon/daemon.go` runs every provider through the `repl` agent backend (`server/pkg/agent/repl.go`), wiring `agent.Config.ReplBroker`; the daemon still claims, heartbeats, checks out, and completes/fails the task.
- `server/cmd/multica/cmd_repo.go` registers `repo checkout <url> [--ref]`.
- `repo checkout` requires `MULTICA_DAEMON_PORT`, sends `workspace_id`, `workdir`, `ref`, `agent_name`, and `task_id` to local daemon `/repo/checkout`, then prints the checked-out path.
- `server/cmd/server/router.go` registers daemon APIs under `/api/daemon`, including workspace repos and task claim.
- `server/internal/daemon/daemon.go` claims tasks, prepares workdirs, launches provider CLIs, and reports completion.
- `server/internal/daemon/execenv/runtime_config.go` injects task/project/repo context into agent workdirs.
