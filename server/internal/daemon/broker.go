package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
//
// Lease / visibility timeout: a REPL session has no liveness channel back to
// the broker — once Next hands it a job, the session goes off to work with no
// open connection. If that session dies or its result POST never lands, the job
// would otherwise be lost and its Submit would block forever (the backend's
// idle-watchdog heartbeat keeps the daemon from failing it). To recover, every
// delivered job carries a lease deadline; a reaper re-enqueues any in-flight job
// whose lease expires, so another session (or the same one after a restart) can
// claim and finish it. A live session working a long task renews its lease via
// Renew (`multica runtime renew <job-id>`).
type replBroker struct {
	logger *slog.Logger
	lease  time.Duration

	mu      sync.Mutex
	seq     uint64
	queue   []*brokerJob          // jobs waiting for a REPL session to claim them
	waiters []chan *brokerJob     // parked Next calls waiting for a job
	jobs    map[string]*brokerJob // jobID -> job, while queued or in flight
}

// brokerJob is one unit of work moving through the broker. All mutable fields
// are guarded by replBroker.mu.
type brokerJob struct {
	id     string
	task   agent.ReplTask
	result chan agent.Result // buffered(1); Report delivers, Submit receives

	inFlight      bool      // true while claimed by a session and not yet reported
	leaseDeadline time.Time // when an in-flight job becomes reclaimable
}

func newReplBroker(logger *slog.Logger, lease time.Duration) *replBroker {
	if logger == nil {
		logger = slog.Default()
	}
	if lease <= 0 {
		lease = DefaultRuntimeReplLease
	}
	return &replBroker{
		logger: logger,
		lease:  lease,
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
	b.enqueueLocked(job)
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

// enqueueLocked makes job claimable: it hands the job straight to a parked Next
// when one is waiting, otherwise appends it to the queue. Either way the job is
// marked not-in-flight (it is up for grabs). Callers must hold b.mu.
//
// When handing to a waiter, the job goes in-flight immediately and its lease
// starts — the waiter is a Next call that is about to return the job to a
// session. The waiter channel is buffered(1), so the send never blocks here.
func (b *replBroker) enqueueLocked(job *brokerJob) {
	if len(b.waiters) > 0 {
		w := b.waiters[0]
		b.waiters = b.waiters[1:]
		b.markInFlightLocked(job)
		w <- job
		return
	}
	job.inFlight = false
	job.leaseDeadline = time.Time{}
	b.queue = append(b.queue, job)
}

// markInFlightLocked records that a job has been handed to a session and starts
// its lease. Callers must hold b.mu.
func (b *replBroker) markInFlightLocked(job *brokerJob) {
	job.inFlight = true
	job.leaseDeadline = time.Now().Add(b.lease)
}

// Next returns the next job for a REPL session, blocking until one is available
// or ctx is done (the caller bounds this with a long-poll deadline). The second
// return is false when ctx fired with no job to hand out.
func (b *replBroker) Next(ctx context.Context) (*brokerJob, bool) {
	b.mu.Lock()
	if len(b.queue) > 0 {
		job := b.queue[0]
		b.queue = b.queue[1:]
		b.markInFlightLocked(job)
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
			// enqueueLocked pulled us off the waiter list and is delivering a
			// job concurrently — take it instead of dropping it on the floor.
			select {
			case job := <-w:
				return job, true
			default:
			}
		}
		return nil, false
	}
}

// Renew extends the lease of an in-flight job so a live session working a long
// task is not reclaimed out from under it. Returns false when the job is
// unknown or no longer in flight (already reported / cancelled / reclaimed).
func (b *replBroker) Renew(jobID string) bool {
	return b.renewAt(jobID, time.Now())
}

// renewAt is Renew with an explicit clock so tests can drive lease extension
// deterministically.
func (b *replBroker) renewAt(jobID string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	job, ok := b.jobs[jobID]
	if !ok || !job.inFlight {
		return false
	}
	job.leaseDeadline = now.Add(b.lease)
	return true
}

// Report delivers a result for jobID to the waiting Submit. It returns false
// when the job is unknown — already reported, cancelled, or expired and not yet
// re-claimed — so the caller (CLI) can tell the REPL session to move on.
func (b *replBroker) Report(jobID string, res agent.Result) bool {
	b.mu.Lock()
	job, ok := b.jobs[jobID]
	if ok {
		delete(b.jobs, jobID)
		// The reaper may have re-enqueued this job after its lease expired; if
		// the late session still reports, honour it and pull the job back out
		// of the queue so a future Next cannot hand out a job that is gone.
		b.removeFromQueueLocked(jobID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	job.result <- res
	return true
}

// taskIDForJob returns the Multica task id backing an active job, or "" if the
// job is unknown. Used to correlate a job id (the broker's opaque handle) back
// to the task id that transcript/event reporting is keyed on.
func (b *replBroker) taskIDForJob(jobID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if job, ok := b.jobs[jobID]; ok {
		return job.task.TaskID
	}
	return ""
}

// discard drops a job that will never be reported (its Submit ctx was
// cancelled), removing it from both the queue and the in-flight map so a REPL
// session does not later claim a dead job.
func (b *replBroker) discard(jobID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.jobs, jobID)
	b.removeFromQueueLocked(jobID)
}

// reapExpired re-enqueues every in-flight job whose lease has elapsed, using the
// current wall clock. Split from the loop so tests can drive expiry
// deterministically via reapExpiredAt.
func (b *replBroker) reapExpired() int {
	return b.reapExpiredAt(time.Now())
}

// reapExpiredAt re-enqueues in-flight jobs whose lease deadline is at or before
// now and returns how many were recovered. The job's result channel is left
// intact, so the still-blocked Submit transparently waits for whichever session
// next claims and reports it.
func (b *replBroker) reapExpiredAt(now time.Time) int {
	b.mu.Lock()
	var expired []*brokerJob
	for _, job := range b.jobs {
		if job.inFlight && !job.leaseDeadline.After(now) {
			expired = append(expired, job)
		}
	}
	for _, job := range expired {
		b.enqueueLocked(job)
	}
	n := len(expired)
	b.mu.Unlock()

	for _, job := range expired {
		b.logger.Warn("repl broker: lease expired, re-enqueued abandoned job", "job", job.id, "task", shortID(job.task.TaskID))
	}
	return n
}

// runReaper periodically reclaims jobs whose lease has expired until ctx is
// done. Started by the daemon when running in repl executor mode.
func (b *replBroker) runReaper(ctx context.Context) {
	interval := b.lease / 4
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reapExpired()
		}
	}
}

// removeFromQueueLocked drops jobID from the pending queue if present. Callers
// must hold b.mu.
func (b *replBroker) removeFromQueueLocked(jobID string) {
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
