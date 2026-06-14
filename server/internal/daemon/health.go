package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/repocache"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// HealthResponse is returned by the daemon's local health endpoint.
type HealthResponse struct {
	Status string `json:"status"`
	PID    int    `json:"pid"`
	// OS is the daemon's runtime.GOOS. The desktop app compares it against its
	// own host OS to detect a daemon it cannot manage — e.g. a Windows desktop
	// reaching a Linux daemon inside WSL2 over localhost forwarding. The
	// lifecycle CLI (`daemon start/stop`) acts on the host process namespace,
	// so a foreign-OS daemon can't be started/stopped by the app even though
	// /health is reachable. See #3916.
	OS              string            `json:"os"`
	Uptime          string            `json:"uptime"`
	DaemonID        string            `json:"daemon_id"`
	DeviceName      string            `json:"device_name"`
	ServerURL       string            `json:"server_url"`
	CLIVersion      string            `json:"cli_version"`
	ActiveTaskCount int64             `json:"active_task_count"`
	Agents          []string          `json:"agents"`
	Workspaces      []healthWorkspace `json:"workspaces"`
}

type healthWorkspace struct {
	ID       string   `json:"id"`
	Runtimes []string `json:"runtimes"`
}

// listenHealth binds the health port. Returns the listener or an error if
// another daemon is already running (port taken).
func (d *Daemon) listenHealth() (net.Listener, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", d.cfg.HealthPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("another daemon is already running on %s: %w", addr, err)
	}
	return ln, nil
}

// Long-poll bounds for GET /runtime/next. The REPL session asks for the next
// task; the broker holds the request open up to the wait window so an idle
// session does not busy-loop, then returns an empty body so the caller can ask
// again. The caller may shorten the window with ?wait=<seconds>.
const (
	defaultRuntimeNextWait = 25 * time.Second
	maxRuntimeNextWait     = 60 * time.Second
)

// runtimeNextResponse is the body of GET /runtime/next. Job is nil when the
// wait window elapsed with no task to hand out.
type runtimeNextResponse struct {
	Job *runtimeJob `json:"job"`
}

// runtimeJob is the REPL-facing view of a brokered task.
type runtimeJob struct {
	JobID      string `json:"job_id"`
	TaskID     string `json:"task_id,omitempty"`
	Cwd        string `json:"cwd"`
	Prompt     string `json:"prompt"`
	Model      string `json:"model,omitempty"`
	ThreadName string `json:"thread_name,omitempty"`
}

// runtimeResultRequest is the body of POST /runtime/result.
type runtimeResultRequest struct {
	JobID   string `json:"job_id"`
	Status  string `json:"status"` // "completed" (default) or "failed"
	Summary string `json:"summary"`
	Error   string `json:"error,omitempty"`
}

// runtimeRenewRequest is the body of POST /runtime/renew.
type runtimeRenewRequest struct {
	JobID string `json:"job_id"`
}

// repoCheckoutRequest is the body of a POST /repo/checkout request.
type repoCheckoutRequest struct {
	URL         string `json:"url"`
	WorkspaceID string `json:"workspace_id"`
	WorkDir     string `json:"workdir"`
	Ref         string `json:"ref,omitempty"`
	AgentName   string `json:"agent_name"`
	TaskID      string `json:"task_id"`
}

// healthHandler returns the /health HTTP handler. Extracted from serveHealth
// so tests can exercise it without spinning up a listener.
func (d *Daemon) healthHandler(startedAt time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		var wsList []healthWorkspace
		for id, ws := range d.workspaces {
			wsList = append(wsList, healthWorkspace{
				ID:       id,
				Runtimes: ws.runtimeIDs,
			})
		}
		d.mu.Unlock()

		agents := make([]string, 0, len(d.cfg.Agents))
		for name := range d.cfg.Agents {
			agents = append(agents, name)
		}

		// "starting" until preflight (PAT renew + initial workspace sync +
		// runtime registration) completes; "running" once the daemon can
		// actually claim tasks. The health port is bound before preflight for
		// liveness/diagnostics, so callers must not treat a reachable endpoint
		// as ready — they gate on this status. Consumers that only know
		// "running" (older CLI/desktop) safely treat "starting" as not-ready.
		status := "starting"
		if d.ready.Load() {
			status = "running"
		}

		resp := HealthResponse{
			Status:          status,
			PID:             os.Getpid(),
			OS:              runtime.GOOS,
			Uptime:          time.Since(startedAt).Truncate(time.Second).String(),
			DaemonID:        d.cfg.DaemonID,
			DeviceName:      d.cfg.DeviceName,
			ServerURL:       d.cfg.ServerBaseURL,
			CLIVersion:      d.cfg.CLIVersion,
			ActiveTaskCount: d.activeTasks.Load(),
			Agents:          agents,
			Workspaces:      wsList,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

// shutdownHandler triggers a graceful daemon shutdown by cancelling the
// top-level context. Used by `multica daemon stop` so we don't depend on
// OS-signal delivery, which is unreliable on Windows once the daemon is
// spawned with DETACHED_PROCESS (no shared console with the stop caller).
// The listener is bound to 127.0.0.1 only, so only local processes can hit
// this endpoint.
func (d *Daemon) shutdownHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "shutting down"})
		if d.cancelFunc != nil {
			// Cancel asynchronously so the response flushes first; otherwise
			// srv.Close() races with the writer.
			go d.cancelFunc()
		}
	}
}

// serveHealth runs the health HTTP server on the given listener.
// Blocks until ctx is cancelled.
func (d *Daemon) serveHealth(ctx context.Context, ln net.Listener, startedAt time.Time) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", d.healthHandler(startedAt))
	mux.HandleFunc("/shutdown", d.shutdownHandler())

	mux.HandleFunc("/runtime/next", d.runtimeNextHandler)
	mux.HandleFunc("/runtime/result", d.runtimeResultHandler)
	mux.HandleFunc("/runtime/renew", d.runtimeRenewHandler)

	mux.HandleFunc("/repo/checkout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req repoCheckoutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.URL == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		if req.WorkspaceID == "" {
			http.Error(w, "workspace_id is required", http.StatusBadRequest)
			return
		}
		if req.WorkDir == "" {
			http.Error(w, "workdir is required", http.StatusBadRequest)
			return
		}

		if d.repoCache == nil {
			http.Error(w, "repo cache not initialized", http.StatusInternalServerError)
			return
		}

		if err := d.ensureRepoReady(r.Context(), req.WorkspaceID, req.URL); err != nil {
			statusCode := http.StatusInternalServerError
			if errors.Is(err, ErrRepoNotConfigured) {
				statusCode = http.StatusBadRequest
			}
			d.logger.Error("repo checkout readiness failed", "workspace_id", req.WorkspaceID, "url", req.URL, "error", err)
			http.Error(w, err.Error(), statusCode)
			return
		}

		result, err := d.repoCache.CreateWorktree(repocache.WorktreeParams{
			WorkspaceID:         req.WorkspaceID,
			RepoURL:             req.URL,
			WorkDir:             req.WorkDir,
			Ref:                 req.Ref,
			AgentName:           req.AgentName,
			TaskID:              req.TaskID,
			CoAuthoredByEnabled: d.workspaceCoAuthoredByEnabled(req.WorkspaceID),
		})
		if err != nil {
			d.logger.Error("repo checkout failed", "url", req.URL, "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

	srv := &http.Server{Handler: mux}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	d.logger.Info("health server listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		d.logger.Warn("health server error", "error", err)
	}
}

// runtimeNextHandler long-polls the broker for the next task and returns it to a
// REPL session. Only meaningful when the daemon runs with --executor repl.
func (d *Daemon) runtimeNextHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.replBroker == nil {
		http.Error(w, "daemon is not running in repl executor mode (start it with --executor repl or `multica runtime repl`)", http.StatusServiceUnavailable)
		return
	}

	wait := defaultRuntimeNextWait
	if v := strings.TrimSpace(r.URL.Query().Get("wait")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			wait = time.Duration(secs) * time.Second
		}
	}
	if wait > maxRuntimeNextWait {
		wait = maxRuntimeNextWait
	}

	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()

	job, ok := d.replBroker.Next(ctx)
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		json.NewEncoder(w).Encode(runtimeNextResponse{Job: nil})
		return
	}
	json.NewEncoder(w).Encode(runtimeNextResponse{Job: &runtimeJob{
		JobID:      job.id,
		TaskID:     job.task.TaskID,
		Cwd:        job.task.Cwd,
		Prompt:     job.task.Prompt,
		Model:      job.task.Model,
		ThreadName: job.task.ThreadName,
	}})
}

// runtimeResultHandler accepts a REPL session's result for a brokered job and
// routes it back to the waiting executor.
func (d *Daemon) runtimeResultHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.replBroker == nil {
		http.Error(w, "daemon is not running in repl executor mode", http.StatusServiceUnavailable)
		return
	}

	var req runtimeResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.JobID) == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}

	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "completed"
	}
	if status != "completed" && status != "failed" {
		http.Error(w, "status must be 'completed' or 'failed'", http.StatusBadRequest)
		return
	}

	delivered := d.replBroker.Report(req.JobID, agent.Result{
		Status: status,
		Output: req.Summary,
		Error:  req.Error,
	})

	w.Header().Set("Content-Type", "application/json")
	if !delivered {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"delivered": false,
			"message":   "unknown job id (already reported, cancelled, or expired)",
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"delivered": true})
}

// runtimeRenewHandler extends the lease on an in-flight job so a live REPL
// session working a long task is not reclaimed by the broker's reaper.
func (d *Daemon) runtimeRenewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.replBroker == nil {
		http.Error(w, "daemon is not running in repl executor mode", http.StatusServiceUnavailable)
		return
	}

	var req runtimeRenewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.JobID) == "" {
		http.Error(w, "job_id is required", http.StatusBadRequest)
		return
	}

	renewed := d.replBroker.Renew(req.JobID)
	w.Header().Set("Content-Type", "application/json")
	if !renewed {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"renewed": false,
			"message": "unknown or no-longer-in-flight job id",
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"renewed": true})
}
