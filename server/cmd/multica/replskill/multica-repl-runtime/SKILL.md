---
name: multica-repl-runtime
description: "Use inside an interactive Claude Code REPL to act as a Multica runtime: claim queued Multica tasks from the local daemon broker, run each one in its prepared workdir, and report the result — so work runs on your interactive subscription instead of the metered headless path. Trigger when the user says start the runtime, pick up Multica tasks, or run as a REPL runtime."
allowed-tools: Bash(multica *)
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

## The loop

Repeat these steps until the user asks you to stop:

1. **Claim the next task** (long-polls up to ~25s, then returns `{"job": null}`):

   ```bash
   multica runtime next --output json
   ```

2. **If `job` is null**, no task is queued. Run `multica runtime next` again.
   Do not spin without pausing if the user wants you to wait quietly.

3. **If `job` is present**, it has this shape:

   ```json
   {"job": {"job_id": "job-3", "task_id": "...", "cwd": "/abs/workdir",
            "prompt": "<the full task brief>", "thread_name": "..."}}
   ```

   - `cd` into `cwd` — the daemon already prepared the repo checkout, skills,
     `CLAUDE.md`, and `.multica/project/resources.json` there.
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

4. **Report the result**, passing back the `job_id` from step 3:

   ```bash
   multica runtime result <job_id> --status completed --summary "One-line outcome"
   ```

   Use `--status failed --error "what went wrong"` if you could not complete it.
   For a multi-line summary use `--summary-stdin` (pipe a heredoc) or
   `--summary-file <path>`. The summary is the run's recorded output, not a
   substitute for the issue comment the prompt asked you to post.

5. Go back to step 1 for the next task.

## Concurrency

Each REPL session handles **one task at a time** — you only call `multica
runtime next` again after reporting the current one. To run tasks in parallel,
the user opens additional `claude` REPL sessions; each claims its own task from
the same broker. There is no global one-at-a-time cap.

## Notes

- A task whose `job_id` is unknown when you report (the daemon was restarted, the
  task was cancelled server-side, or your lease expired and the task was already
  recovered by another session) returns `{"delivered": false}`. That is not an
  error in your work — just move on to the next task.
- All Multica platform interaction goes through the `multica` CLI. Do not call
  Multica HTTP endpoints directly.

More source-backed detail: `references/repl-runtime-source-map.md`.
