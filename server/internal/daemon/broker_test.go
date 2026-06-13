package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestReplBrokerSubmitNextReport(t *testing.T) {
	b := newReplBroker(nil)

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
	b := newReplBroker(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := b.Next(ctx); ok {
		t.Fatal("Next should return false when no job arrives before ctx deadline")
	}
}

func TestReplBrokerSubmitCancelled(t *testing.T) {
	b := newReplBroker(nil)
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
	b := newReplBroker(nil)
	if b.Report("job-999", agent.Result{Status: "completed"}) {
		t.Fatal("Report should return false for an unknown job id")
	}
}

// TestReplBrokerConcurrentSessions verifies that N concurrent Submit calls are
// each served by exactly one Next — modelling N parallel REPL sessions, one
// task per session, with no global one-at-a-time cap.
func TestReplBrokerConcurrentSessions(t *testing.T) {
	b := newReplBroker(nil)
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
