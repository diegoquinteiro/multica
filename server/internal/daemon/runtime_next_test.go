package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

// TestRuntimeNextHandlerSplitsJobAndAuth verifies that /runtime/next returns the
// task-scoped credential in a separate `auth` object — never folded into the
// human-facing `job` — so the REPL CLI can persist it without the token ever
// appearing in what the skill prints. The daemon port comes from cfg.HealthPort.
func TestRuntimeNextHandlerSplitsJobAndAuth(t *testing.T) {
	d := &Daemon{
		cfg:        Config{HealthPort: 19514},
		replBroker: newReplBroker(slog.Default(), time.Minute),
		logger:     slog.Default(),
	}

	go func() {
		_, _ = d.replBroker.Submit(context.Background(), agent.ReplTask{
			TaskID:      "task-1",
			Cwd:         "/tmp/wd",
			Prompt:      "do it",
			AuthToken:   "mat_secret",
			AgentID:     "agent-1",
			WorkspaceID: "ws-1",
			AgentName:   "Programador",
		})
	}()

	req := httptest.NewRequest(http.MethodGet, "/runtime/next?wait=2", nil)
	rec := httptest.NewRecorder()
	d.runtimeNextHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Job  map[string]any `json:"job"`
		Auth map[string]any `json:"auth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The job object must NOT carry the token.
	if _, leaked := resp.Job["token"]; leaked {
		t.Fatal("token leaked into the job object")
	}
	if resp.Job["cwd"] != "/tmp/wd" {
		t.Fatalf("job.cwd = %v, want /tmp/wd", resp.Job["cwd"])
	}

	// The auth object carries the credential the CLI persists per job.
	if resp.Auth == nil {
		t.Fatal("auth object missing")
	}
	if resp.Auth["token"] != "mat_secret" {
		t.Fatalf("auth.token = %v, want mat_secret", resp.Auth["token"])
	}
	if resp.Auth["agent_id"] != "agent-1" {
		t.Fatalf("auth.agent_id = %v, want agent-1", resp.Auth["agent_id"])
	}
	if resp.Auth["workspace_id"] != "ws-1" {
		t.Fatalf("auth.workspace_id = %v, want ws-1", resp.Auth["workspace_id"])
	}
	if resp.Auth["daemon_port"] != float64(19514) {
		t.Fatalf("auth.daemon_port = %v, want 19514", resp.Auth["daemon_port"])
	}
}
