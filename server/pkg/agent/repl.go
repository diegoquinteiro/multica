package agent

import (
	"context"
	"fmt"
	"time"
)

// replHeartbeatInterval is how often the repl backend emits a status message
// while a task is parked in the broker (queued or being worked on by a human
// REPL session). The daemon's idle watchdog force-stops a run that emits no
// message for AgentIdleWatchdog (default 30m); a REPL session has no
// progress channel back to the daemon, so without these beats a legitimate
// long task would be killed mid-run. Keep it well under the watchdog window.
const replHeartbeatInterval = 60 * time.Second

// replBackend implements Backend by handing the prepared task to a daemon-side
// broker instead of spawning a subprocess. A human-launched Claude Code REPL
// session claims the task (`multica runtime next`), runs it with its native
// tools, and reports the outcome (`multica runtime result`). This keeps the
// execution on the user's interactive subscription quota rather than the
// metered Agent SDK / `claude -p` path, while the daemon keeps owning the full
// task lifecycle (register, claim, heartbeat, checkout, complete/fail).
type replBackend struct {
	cfg Config
}

func (b *replBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	if b.cfg.ReplBroker == nil {
		return nil, fmt.Errorf("repl backend requires a broker; start the daemon with --executor repl")
	}

	task := ReplTask{
		TaskID:     opts.TaskID,
		Cwd:        opts.Cwd,
		Prompt:     prompt,
		Model:      opts.Model,
		ThreadName: opts.ThreadName,
		// The daemon builds the full agent env (token, agent id, ...) before the
		// backend split, so the task-scoped context is already here. Forward it
		// so the REPL session can authenticate and scope as the agent.
		AuthToken:   b.cfg.Env["MULTICA_TOKEN"],
		AgentID:     b.cfg.Env["MULTICA_AGENT_ID"],
		WorkspaceID: b.cfg.Env["MULTICA_WORKSPACE_ID"],
		AgentName:   b.cfg.Env["MULTICA_AGENT_NAME"],
	}

	msgCh := make(chan Message, 8)
	resCh := make(chan Result, 1)

	go func() {
		defer close(msgCh)
		defer close(resCh)

		start := time.Now()

		// Keep the daemon's idle watchdog satisfied while we block on the
		// broker. The drain loop refreshes its activity timer on every message,
		// so a periodic status beat is enough; it carries no progress detail
		// because the daemon cannot observe what the REPL session is doing.
		beatCtx, stopBeats := context.WithCancel(ctx)
		defer stopBeats()
		go func() {
			ticker := time.NewTicker(replHeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-beatCtx.Done():
					return
				case <-ticker.C:
					trySend(msgCh, Message{Type: MessageStatus, Status: "waiting for repl session"})
				}
			}
		}()

		trySend(msgCh, Message{Type: MessageStatus, Status: "queued for repl session"})

		result, err := b.cfg.ReplBroker.Submit(ctx, task)
		stopBeats()

		if err != nil {
			if result.Status == "" {
				if ctx.Err() != nil {
					result.Status = "aborted"
				} else {
					result.Status = "failed"
				}
			}
			if result.Error == "" {
				result.Error = err.Error()
			}
		}
		if result.Status == "" {
			result.Status = "completed"
		}
		result.DurationMs = time.Since(start).Milliseconds()

		resCh <- result
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}
