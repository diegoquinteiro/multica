---
name: multica-repl-runtime
description: "Use inside an interactive Claude Code REPL to act as a Multica runtime: claim queued Multica tasks from the local daemon broker, run each one in its prepared workdir, and report the result — so work runs on your interactive subscription instead of the metered headless path. Trigger when the user says start the runtime, pick up Multica tasks, or run as a REPL runtime."
allowed-tools: Bash(multica *), ScheduleWakeup
---

# Multica REPL Runtime

Turn this interactive Claude Code session into a Multica runtime. The local
daemon (started with `multica runtime repl` or `multica daemon start --executor
repl`) keeps owning registration, task claim, heartbeat, and repo checkout. This
session only **executes** tasks: it pulls each prepared task from the daemon's
local broker, does the work, and reports the outcome. Because the work runs in
this REPL, it draws on your interactive subscription quota — no second `claude`
subprocess is spawned.

## Prerequisites

The daemon must be running in repl executor mode on this machine. If `multica
runtime next` returns a "not running in repl executor mode" error, tell the user
to start it with `multica runtime repl` (or `multica daemon start --executor
repl`) and stop.

## Zero idle cost

`multica runtime next --block` holds the connection open inside the CLI and
**only returns to you when a real job is claimed**. The wait is a blocking
network call — it costs you no tokens while it waits. So while the queue is
empty you spend essentially nothing: one model turn per actual job, not one per
poll. Never replace `--block` with a busy loop of plain `next` calls — that
re-wakes the model for every empty poll and burns tokens for no work.

## The loop

Repeat these steps until the user asks you to stop:

1. **Arm the watchdog, then claim the next task.** Before blocking, schedule a
   safety-net wakeup (see *Resilience* below) so the loop self-heals if this
   session ever stops. Then claim:

   ```bash
   multica runtime next --block --output json
   ```

   This blocks (no token cost) until a job is ready, then prints
   `{"job": {...}}`. It returns without a job only if interrupted (Ctrl-C) — if
   you ever get `{"job": null}`, the user stopped the runtime; do not re-loop.

2. **The job** has this shape:

   ```json
   {"job": {"job_id": "job-3", "task_id": "...", "cwd": "/abs/workdir",
            "prompt": "<the full task brief>", "thread_name": "..."}}
   ```

   - `cd` into `cwd` — the daemon already prepared the repo checkout, skills,
     `CLAUDE.md`, and `.multica/project/resources.json` there. **Run every
     `multica` command for this job from inside `cwd`**: that is where the job's
     credential lives, so commands there authenticate as the assigned agent
     (this is what lets `squad activity`, `repo checkout`, and other
     agent-scoped commands work — outside `cwd` they fall back to your own login
     and may be refused).
   - Treat `prompt` as your task instructions and carry them out fully with your
     native tools, exactly as you would a normal Multica task. The prompt tells
     you which issue to read and what to do; follow it, including posting any
     result comment or opening a PR it asks for.
   - **Keep the lease alive on long work.** A claimed task has a lease (default
     30 min). If you neither report nor renew within it, the daemon assumes this
     session died and re-enqueues the task for another session — which would run
     it twice. For any task that may take a while, run `multica runtime renew
     <job_id>` periodically (e.g. before each long build/test step):

     ```bash
     multica runtime renew <job_id>
     ```

3. **Report the result**, passing back the `job_id` from step 2:

   ```bash
   multica runtime result <job_id> --status completed --summary "One-line outcome"
   ```

   Use `--status failed --error "what went wrong"` if you could not complete it.
   For a multi-line summary use `--summary-stdin` (pipe a heredoc) or
   `--summary-file <path>`. The summary is the run's recorded output, not a
   substitute for the issue comment the prompt asked you to post. Reporting the
   result also clears the job's credential from `cwd`.

4. Go back to step 1 for the next task.

## Resilience (watchdog)

The blocking `next --block` is the normal idle state, but a session can still
stop unexpectedly (model ends its turn, a transient error, a crash). To keep the
runtime alive without you babysitting it, arm a **watchdog wakeup** with
`ScheduleWakeup` before each blocking wait:

- Call `ScheduleWakeup` with a long delay (e.g. `delaySeconds: 1800`) and a
  `prompt` that re-invokes this skill (`/multica-repl-runtime`). Each loop
  iteration re-arms it. While the loop is healthy this wakeup is simply replaced
  by the next iteration's; if the session ever stops, the pending wakeup fires
  and restarts the claim loop. The delay only needs to be long enough not to
  interrupt a normal blocking wait — the broker's long-poll window is minutes,
  so ~1800s is a good safety net.

### Stopping cleanly

When the user asks you to stop the runtime:

- **Do not** schedule another wakeup, and **do not** call `next` again.
- If a wakeup is already pending, cancel it (omit the next `ScheduleWakeup` —
  not re-arming is what ends the loop) so the runtime does not silently resume.
- Acknowledge that the runtime is stopped and end your turn.

If `ScheduleWakeup` is unavailable in your harness, fall back to re-running
`multica runtime next --block` after each job (the textual loop) — you lose the
auto-restart safety net but the per-job blocking behavior is unchanged.

## Concurrency

Each REPL session handles **one task at a time** — you only call `multica
runtime next` again after reporting the current one. To run tasks in parallel,
the user opens additional `claude` REPL sessions; each claims its own task from
the same broker. There is no global one-at-a-time cap. Each job's credential is
scoped to its own `cwd`, so parallel sessions never collide.

## Notes

- A task whose `job_id` is unknown when you report (the daemon was restarted, the
  task was cancelled server-side, or your lease expired and the task was already
  recovered by another session) returns `{"delivered": false}`. That is not an
  error in your work — just move on to the next task.
- All Multica platform interaction goes through the `multica` CLI. Do not call
  Multica HTTP endpoints directly.

More source-backed detail: `references/repl-runtime-source-map.md`.
