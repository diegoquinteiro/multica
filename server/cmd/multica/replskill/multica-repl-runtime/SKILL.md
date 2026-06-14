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
            "prompt": "<the full task brief>", "model": "claude-...",
            "thread_name": "..."}}
   ```

   - `cd` into `cwd` — the daemon already prepared the repo checkout, skills,
     `CLAUDE.md`, and `.multica/project/resources.json` there. **Run every
     `multica` command for this job from inside `cwd`**: that is where the job's
     credential lives, so commands there authenticate as the assigned agent
     (this is what lets `squad activity`, `repo checkout`, and other
     agent-scoped commands work — outside `cwd` they fall back to your own login
     and may be refused).

3. **Run the task in a sub-agent at the agent's model.** The job's `model` is the
   model configured for the assigned agent. Honor it by running the work in a
   sub-agent (the Agent tool) with `model` set from it, instead of in this loop's
   own model:

   - Map `model` → tier: contains `haiku` → `haiku`, `sonnet` → `sonnet`,
     `opus` → `opus`, `fable` → `fable`. If `model` is empty or unrecognized,
     omit `model` so the sub-agent inherits this session's.
   - Spawn **one** sub-agent, run from `cwd`, whose prompt is the job's `prompt`
     **verbatim**. Also tell the sub-agent: it is acting as the assigned Multica
     agent; run every `multica` command from `cwd`; and on long work call
     `multica runtime renew <job_id>` periodically (the lease is ~30 min — if
     nothing renews or reports within it, the daemon re-enqueues the task and it
     runs twice).
   - The sub-agent carries out the task fully — reads the issue and posts the
     result comment / opens the PR the prompt asks for. Its final message is the
     outcome you report in step 4.

   Why a sub-agent: it lets each task run at its agent's configured model while
   this loop stays on your default model. The sub-agent's activity still streams
   to the Multica UI (see *Live activity*).

4. **Report the result**, passing back the `job_id` from step 2:

   ```bash
   multica runtime result <job_id> --status completed --summary "One-line outcome"
   ```

   Use `--status failed --error "what went wrong"` if you could not complete it.
   For a multi-line summary use `--summary-stdin` (pipe a heredoc) or
   `--summary-file <path>`. The summary is the run's recorded output, not a
   substitute for the issue comment the prompt asked you to post. Reporting the
   result also clears the job's credential from `cwd`.

5. Go back to step 1 for the next task.

## Live activity (automatic)

While a job runs, your session's tool calls and messages — **including the
sub-agent's** — are streamed to the task's activity feed in the Multica UI, the
same way a headless run reports. This is automatic: `multica runtime repl`
installs Claude Code hooks that forward the session transcript through the
daemon, so you never call anything for it. Just keep working inside `cwd` so the
hook attributes the activity to the right task.

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
