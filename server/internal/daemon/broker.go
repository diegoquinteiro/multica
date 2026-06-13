package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// replBroker is the in-process handoff between the daemon's repl backend and a
// human-launched Claude Code REPL session. The backend Submits a prepared task;
// a REPL session pulls it via Next (long-polled through the loopback health
// server) and returns the outcome via Report. One Submit is served by exactly
// one Next, so N concurrent REPL sessions process N tasks in parallel (one task
// per session) without any global concurrency cap — the only ceiling is the
// daemon's MaxConcurrentTasks, which bounds how many tasks are claimed and
// parked here at once.
//
// The broker hands out an opaque job id rather than the Multica task id: the
// same task id can be Submitted twice (the daemon retries a failed session
// resume with a fresh session), so the task id is not a safe correlation key.
type replBroker struct {
	logger *slog.Logger

	mu      sync.Mutex
	seq     uint64
	queue   []*brokerJob          // jobs waiting for a REPL session to claim them
	waiters []chan *brokerJob     // parked Next calls waiting for a job
	jobs    map[string]*brokerJob // jobID -> job, while queued or in flight
}

// brokerJob is one unit of work moving through the broker.
type brokerJob struct {
	id     string
	task   agent.ReplTask
	result chan agent.Result // buffered(1); Report delivers, Submit receives
}

func newReplBroker(logger *slog.Logger) *replBroker {
	if logger == nil {
		logger = slog.Default()
	}
	return &replBroker{
		logger: logger,
		jobs:   make(map[string]*brokerJob),
	}
}

// Submit enqueues a task and blocks until a REPL session reports a result or
// ctx is cancelled. It implements agent.ReplBroker.
func (b *replBroker) Submit(ctx context.Context, task agent.ReplTask) (agent.Result, error) {
	job := &brokerJob{
		task:   task,
		result: make(chan agent.Result, 1),
	}

	b.mu.Lock()
	b.seq++
	job.id = fmt.Sprintf("job-%d", b.seq)
	b.jobs[job.id] = job
	if len(b.waiters) > 0 {
		// Hand straight to a parked Next. The waiter channel is buffered(1),
		// so the send never blocks under the lock.
		w := b.waiters[0]
		b.waiters = b.waiters[1:]
		w <- job
	} else {
		b.queue = append(b.queue, job)
	}
	queued := len(b.queue)
	b.mu.Unlock()

	b.logger.Info("repl broker: task queued", "job", job.id, "task", shortID(task.TaskID), "thread", task.ThreadName, "queued", queued)

	select {
	case res := <-job.result:
		b.logger.Info("repl broker: result received", "job", job.id, "status", res.Status)
		return res, nil
	case <-ctx.Done():
		b.discard(job.id)
		b.logger.Info("repl broker: task cancelled before result", "job", job.id, "error", ctx.Err())
		return agent.Result{Status: "aborted", Error: "repl task cancelled before a session reported a result"}, ctx.Err()
	}
}

// Next returns the next job for a REPL session, blocking until one is available
// or ctx is done (the caller bounds this with a long-poll deadline). The second
// return is false when ctx fired with no job to hand out.
func (b *replBroker) Next(ctx context.Context) (*brokerJob, bool) {
	b.mu.Lock()
	if len(b.queue) > 0 {
		job := b.queue[0]
		b.queue = b.queue[1:]
		b.mu.Unlock()
		return job, true
	}
	w := make(chan *brokerJob, 1)
	b.waiters = append(b.waiters, w)
	b.mu.Unlock()

	select {
	case job := <-w:
		return job, true
	case <-ctx.Done():
		b.mu.Lock()
		removed := removeWaiter(&b.waiters, w)
		b.mu.Unlock()
		if !removed {
			// Submit pulled us off the waiter list and is delivering a job
			// concurrently — take it instead of dropping it on the floor.
			select {
			case job := <-w:
				return job, true
			default:
			}
		}
		return nil, false
	}
}

// Report delivers a result for jobID to the waiting Submit. It returns false
// when the job is unknown — already reported, cancelled, or never existed — so
// the caller (CLI) can tell the REPL session to move on.
func (b *replBroker) Report(jobID string, res agent.Result) bool {
	b.mu.Lock()
	job, ok := b.jobs[jobID]
	if ok {
		delete(b.jobs, jobID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	job.result <- res
	return true
}

// discard drops a job that will never be reported (its Submit ctx was
// cancelled), removing it from both the queue and the in-flight map so a REPL
// session does not later claim a dead job.
func (b *replBroker) discard(jobID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.jobs, jobID)
	for i, j := range b.queue {
		if j.id == jobID {
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			return
		}
	}
}

// removeWaiter drops w from waiters if present, reporting whether it was found.
func removeWaiter(waiters *[]chan *brokerJob, w chan *brokerJob) bool {
	for i, c := range *waiters {
		if c == w {
			*waiters = append((*waiters)[:i], (*waiters)[i+1:]...)
			return true
		}
	}
	return false
}
