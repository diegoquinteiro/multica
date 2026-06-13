package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stubBroker is a test ReplBroker that records the submitted task and returns a
// canned result (or error).
type stubBroker struct {
	got    ReplTask
	result Result
	err    error
}

func (s *stubBroker) Submit(ctx context.Context, task ReplTask) (Result, error) {
	s.got = task
	return s.result, s.err
}

func drain(t *testing.T, sess *Session) Result {
	t.Helper()
	for range sess.Messages {
		// discard streamed status messages
	}
	select {
	case res := <-sess.Result:
		return res
	case <-time.After(2 * time.Second):
		t.Fatal("no result from repl session")
		return Result{}
	}
}

func TestReplBackendRequiresBroker(t *testing.T) {
	b := &replBackend{cfg: Config{}}
	if _, err := b.Execute(context.Background(), "p", ExecOptions{}); err == nil {
		t.Fatal("Execute should fail without a broker")
	}
}

func TestReplBackendForwardsTaskAndResult(t *testing.T) {
	stub := &stubBroker{result: Result{Status: "completed", Output: "ok"}}
	b := &replBackend{cfg: Config{ReplBroker: stub}}

	sess, err := b.Execute(context.Background(), "the prompt", ExecOptions{
		TaskID:     "task-1",
		Cwd:        "/work",
		Model:      "claude-x",
		ThreadName: "thread",
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	res := drain(t, sess)
	if res.Status != "completed" || res.Output != "ok" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if stub.got.Prompt != "the prompt" || stub.got.TaskID != "task-1" || stub.got.Cwd != "/work" {
		t.Fatalf("broker received wrong task: %+v", stub.got)
	}
	if res.DurationMs < 0 {
		t.Fatalf("duration should be set, got %d", res.DurationMs)
	}
}

func TestReplBackendBrokerErrorMarksAborted(t *testing.T) {
	stub := &stubBroker{err: errors.New("cancelled"), result: Result{Status: "aborted"}}
	b := &replBackend{cfg: Config{ReplBroker: stub}}

	sess, err := b.Execute(context.Background(), "p", ExecOptions{})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	res := drain(t, sess)
	if res.Status != "aborted" {
		t.Fatalf("expected aborted, got %q", res.Status)
	}
	if res.Error == "" {
		t.Fatal("expected error detail to be populated")
	}
}
