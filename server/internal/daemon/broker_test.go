package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestReplBrokerSubmitNextReport(t *testing.T) {
	b := newReplBroker(nil, time.Minute)

	resultCh := make(chan agent.Result, 1)
	go func() {
		res, err := b.Submit(context.Background(), agent.ReplTask{TaskID: "t1", Cwd: "/tmp/w", Prompt: "do it"})
		if err != nil {
			t.Errorf("Submit returned error: %v", err)
		}
		resultCh <- res
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	job, ok := b.Next(ctx)
	if !ok {
		t.Fatal("Next returned no job")
	}
	if job.task.TaskID != "t1" || job.task.Cwd != "/tmp/w" || job.task.Prompt != "do it" {
		t.Fatalf("unexpected job task: %+v", job.task)
	}

	if !b.Report(job.id, agent.Result{Status: "completed", Output: "done"}) {
		t.Fatal("Report returned false for a live job")
	}

	select {
	case res := <-resultCh:
		if res.Status != "completed" || res.Output != "done" {
			t.Fatalf("unexpected result: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit did not return after Report")
	}
}

func TestReplBrokerNextTimeout(t *testing.T) {
	b := newReplBroker(nil, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := b.Next(ctx); ok {
		t.Fatal("Next should return false when no job arrives before ctx deadline")
	}
}

func TestReplBrokerSubmitCancelled(t *testing.T) {
	b := newReplBroker(nil, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		res, err := b.Submit(ctx, agent.ReplTask{TaskID: "t1"})
		if err == nil {
			t.Errorf("Submit should return ctx error when cancelled")
		}
		if res.Status != "aborted" {
			t.Errorf("cancelled Submit should be aborted, got %q", res.Status)
		}
		close(done)
	}()

	// Give Submit a moment to enqueue, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Submit did not return")
	}

	// The cancelled job must have been discarded, so Next finds nothing.
	nctx, ncancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer ncancel()
	if _, ok := b.Next(nctx); ok {
		t.Fatal("discarded job should not be handed out by Next")
	}
}

func TestReplBrokerReportUnknownJob(t *testing.T) {
	b := newReplBroker(nil, time.Minute)
	if b.Report("job-999", agent.Result{Status: "completed"}) {
		t.Fatal("Report should return false for an unknown job id")
	}
}

// TestReplBrokerLeaseExpiryRecoversAbandonedJob covers the Reviewer's case: a
// REPL session claims a job via Next and then dies (never calls Report). The
// lease must expire and the job must become claimable again by another session,
// after which a normal Report completes the original Submit.
func TestReplBrokerLeaseExpiryRecoversAbandonedJob(t *testing.T) {
	b := newReplBroker(nil, 30*time.Minute)

	resultCh := make(chan agent.Result, 1)
	go func() {
		res, _ := b.Submit(context.Background(), agent.ReplTask{TaskID: "t1", Prompt: "do it"})
		resultCh <- res
	}()

	// Session A claims the job, then "dies" without reporting.
	ctxA, cancelA := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelA()
	jobA, ok := b.Next(ctxA)
	if !ok {
		t.Fatal("Next (session A) returned no job")
	}

	// Before the lease elapses, the job is NOT reclaimable.
	if n := b.reapExpiredAt(time.Now()); n != 0 {
		t.Fatalf("job reaped before lease expiry: %d", n)
	}
	ctxEarly, cancelEarly := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelEarly()
	if _, ok := b.Next(ctxEarly); ok {
		t.Fatal("in-flight job should not be handed out before its lease expires")
	}

	// Lease elapses → reaper re-enqueues the abandoned job.
	if n := b.reapExpiredAt(time.Now().Add(31 * time.Minute)); n != 1 {
		t.Fatalf("expected 1 reclaimed job after lease expiry, got %d", n)
	}

	// Session B now claims the recovered job and reports it.
	ctxB, cancelB := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelB()
	jobB, ok := b.Next(ctxB)
	if !ok {
		t.Fatal("Next (session B) did not get the recovered job")
	}
	if jobB.id != jobA.id || jobB.task.TaskID != "t1" {
		t.Fatalf("recovered job mismatch: A=%s B=%s task=%s", jobA.id, jobB.id, jobB.task.TaskID)
	}

	if !b.Report(jobB.id, agent.Result{Status: "completed", Output: "recovered"}) {
		t.Fatal("Report on recovered job returned false")
	}
	select {
	case res := <-resultCh:
		if res.Status != "completed" || res.Output != "recovered" {
			t.Fatalf("unexpected result after recovery: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit did not complete after recovered job was reported")
	}

	// A late Report from the dead session A must be a no-op (job already gone).
	if b.Report(jobA.id, agent.Result{Status: "completed"}) {
		t.Fatal("late Report from the abandoned session should not deliver")
	}
}

// TestReplBrokerRenewPreventsReap covers the live-but-slow session: a session
// that renews its lease must not be reclaimed, while one that doesn't is.
func TestReplBrokerRenewPreventsReap(t *testing.T) {
	b := newReplBroker(nil, 30*time.Minute)

	go b.Submit(context.Background(), agent.ReplTask{TaskID: "t1"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	job, ok := b.Next(ctx)
	if !ok {
		t.Fatal("Next returned no job")
	}

	// Renew at +25m extends the lease to ~+55m (claim deadline was ~+30m).
	base := time.Now()
	if !b.renewAt(job.id, base.Add(25*time.Minute)) {
		t.Fatal("Renew returned false for an in-flight job")
	}
	// Reaping at +31m must now find nothing — renew pushed the deadline past it.
	if n := b.reapExpiredAt(base.Add(31 * time.Minute)); n != 0 {
		t.Fatalf("renewed job should not be reaped at the original deadline, reaped %d", n)
	}
	// Past the renewed deadline (~+55m) it is reclaimable again.
	if n := b.reapExpiredAt(base.Add(56 * time.Minute)); n != 1 {
		t.Fatalf("expected renewed job to be reclaimable past the new deadline, reaped %d", n)
	}

	// After it has been re-enqueued, Renew no longer applies (not in flight).
	if b.Renew(job.id) {
		t.Fatal("Renew should fail for a job that is no longer in flight")
	}
}

// TestReplBrokerConcurrentSessions verifies that N concurrent Submit calls are
// each served by exactly one Next — modelling N parallel REPL sessions, one
// task per session, with no global one-at-a-time cap.
func TestReplBrokerConcurrentSessions(t *testing.T) {
	b := newReplBroker(nil, time.Minute)
	const n = 8

	var submitWG sync.WaitGroup
	for i := 0; i < n; i++ {
		submitWG.Add(1)
		go func(i int) {
			defer submitWG.Done()
			if _, err := b.Submit(context.Background(), agent.ReplTask{TaskID: "task"}); err != nil {
				t.Errorf("Submit %d failed: %v", i, err)
			}
		}(i)
	}

	seen := make(map[string]struct{})
	var mu sync.Mutex
	var nextWG sync.WaitGroup
	for i := 0; i < n; i++ {
		nextWG.Add(1)
		go func() {
			defer nextWG.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			job, ok := b.Next(ctx)
			if !ok {
				t.Errorf("Next returned no job")
				return
			}
			mu.Lock()
			seen[job.id] = struct{}{}
			mu.Unlock()
			b.Report(job.id, agent.Result{Status: "completed"})
		}()
	}

	nextWG.Wait()
	submitWG.Wait()

	if len(seen) != n {
		t.Fatalf("expected %d distinct jobs claimed, got %d", n, len(seen))
	}
}
