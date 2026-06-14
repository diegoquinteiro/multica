package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// replSkillFS embeds the multica-repl-runtime skill so `multica runtime repl`
// can install it into the user's interactive Claude Code skills directory. This
// skill drives a human-launched REPL session, so it is NOT shipped through the
// agent built-in skills path (server/internal/service/builtin_skills) — those
// go to headless daemon-spawned agents, for which this loop is meaningless.
//
//go:embed replskill
var replSkillFS embed.FS

const (
	replSkillName    = "multica-repl-runtime"
	replSkillFSRoot  = "replskill/" + replSkillName
	replSkillDirPerm = 0o755
)

var runtimeReplCmd = &cobra.Command{
	Use:   "repl",
	Short: "Run this machine as a Multica runtime driven by interactive Claude Code REPL sessions",
	Long: "Install the multica-repl-runtime skill and start the daemon in repl executor mode.\n\n" +
		"In this mode the daemon still registers, claims tasks, heartbeats, and checks out repos,\n" +
		"but each task is executed by a human-launched `claude` REPL session that loads the\n" +
		"multica-repl-runtime skill — so work runs on your interactive subscription quota.\n\n" +
		"This command runs the daemon in the foreground. Open `claude` in another terminal and\n" +
		"ask it to start the runtime (the multica-repl-runtime skill drives the claim loop). Open\n" +
		"more `claude` sessions to process tasks in parallel — one task per session.",
	RunE: runRuntimeRepl,
}

var runtimeNextCmd = &cobra.Command{
	Use:   "next",
	Short: "Claim the next task from the local repl broker (long-polls)",
	RunE:  runRuntimeNext,
}

var runtimeResultCmd = &cobra.Command{
	Use:   "result <job-id>",
	Short: "Report the result of a brokered repl task",
	Args:  exactArgs(1),
	RunE:  runRuntimeResult,
}

var runtimeRenewCmd = &cobra.Command{
	Use:   "renew <job-id>",
	Short: "Extend the lease on an in-flight repl task (call during long work so it is not reclaimed)",
	Args:  exactArgs(1),
	RunE:  runRuntimeRenew,
}

func init() {
	runtimeCmd.AddCommand(runtimeReplCmd)
	runtimeCmd.AddCommand(runtimeNextCmd)
	runtimeCmd.AddCommand(runtimeResultCmd)
	runtimeCmd.AddCommand(runtimeRenewCmd)

	runtimeReplCmd.Flags().Bool("skip-skill-install", false, "Do not (re)install the multica-repl-runtime skill into ~/.claude/skills")

	runtimeNextCmd.Flags().Int("wait", 300, "Seconds to long-poll for a task before returning {\"job\": null} (max 1800)")
	runtimeNextCmd.Flags().Bool("block", false, "Keep re-polling internally and only return once a real job is claimed (one model turn per job, no idle wake-ups)")
	runtimeNextCmd.Flags().String("output", "json", "Output format: json")

	runtimeResultCmd.Flags().String("status", "completed", "Task outcome: completed or failed")
	runtimeResultCmd.Flags().String("summary", "", "Short outcome summary (decodes \\n, \\r, \\t, \\\\)")
	runtimeResultCmd.Flags().Bool("summary-stdin", false, "Read the summary from stdin (verbatim, multi-line)")
	runtimeResultCmd.Flags().String("summary-file", "", "Read the summary from a UTF-8 file (verbatim, multi-line)")
	runtimeResultCmd.Flags().String("error", "", "Error detail when --status failed")
	runtimeResultCmd.Flags().String("output", "json", "Output format: json")

	runtimeRenewCmd.Flags().String("output", "json", "Output format: json")
}

// daemonLoopbackPort resolves the local daemon's loopback port: an explicit
// MULTICA_DAEMON_PORT (set inside daemon-spawned tasks) wins, then the port
// persisted by `runtime next` for the current repl job (so commands like
// `repo checkout` work inside a REPL session, which gets no env from the
// daemon), otherwise the health port for the active profile — the same port
// `daemon status` probes.
func daemonLoopbackPort(cmd *cobra.Command) int {
	if v := os.Getenv("MULTICA_DAEMON_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	if ja := loadJobAuth(); ja.DaemonPort > 0 {
		return ja.DaemonPort
	}
	return healthPortForProfile(resolveProfile(cmd))
}

// --- repl job credentials ---
//
// In repl executor mode the daemon does not spawn a subprocess, so the
// task-scoped credential and daemon port it would normally inject as env never
// reach the human-launched REPL session. `runtime next` persists them into the
// job's prepared workdir (<cwd>/.multica/job-auth.json, mode 0600); the env
// resolvers fall back to this file so every independent `multica` invocation in
// the job authenticates as the agent — fixing the 403 on agent-only endpoints
// (e.g. `squad activity`) and unblocking `repo checkout`. `runtime result`
// removes the file when the job ends.

const jobAuthRelPath = ".multica/job-auth.json"

type jobAuth struct {
	Token       string `json:"token,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	AgentName   string `json:"agent_name,omitempty"`
	DaemonPort  int    `json:"daemon_port,omitempty"`
}

// findJobAuthFile walks up from the current working directory looking for the
// nearest .multica/job-auth.json. Returns "" outside a repl job (the common
// case for ordinary CLI use).
func findJobAuthFile() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, jobAuthRelPath)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// loadJobAuth reads the nearest job-auth file, or returns a zero value when
// there is none or it cannot be parsed (callers then fall back to env/profile).
func loadJobAuth() jobAuth {
	path := findJobAuthFile()
	if path == "" {
		return jobAuth{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return jobAuth{}
	}
	var ja jobAuth
	_ = json.Unmarshal(data, &ja)
	return ja
}

// writeJobAuth persists the job credential into the job's prepared workdir.
func writeJobAuth(cwd string, ja jobAuth) error {
	dir := filepath.Join(cwd, ".multica")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(ja)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "job-auth.json"), data, 0o600)
}

// removeJobAuth deletes the nearest job-auth file (best effort) when a job ends.
func removeJobAuth() {
	if path := findJobAuthFile(); path != "" {
		_ = os.Remove(path)
	}
}

// --- runtime repl ---

func runRuntimeRepl(cmd *cobra.Command, _ []string) error {
	skipInstall, _ := cmd.Flags().GetBool("skip-skill-install")
	if !skipInstall {
		dest, err := installReplSkill()
		if err != nil {
			return fmt.Errorf("install %s skill: %w", replSkillName, err)
		}
		fmt.Fprintf(os.Stderr, "Installed %s skill → %s\n", replSkillName, dest)
	}

	fmt.Fprintln(os.Stderr, "Starting daemon in repl executor mode (foreground).")
	fmt.Fprintln(os.Stderr, "In another terminal, run `claude` and ask it to start the Multica runtime;")
	fmt.Fprintln(os.Stderr, "the multica-repl-runtime skill claims and runs queued tasks. Open more `claude`")
	fmt.Fprintln(os.Stderr, "sessions to process tasks in parallel (one task per session). Ctrl-C to stop.")

	return startDaemonForeground(cmd, "repl")
}

// installReplSkill writes the embedded multica-repl-runtime skill into the
// user's Claude Code skills directory (~/.claude/skills/multica-repl-runtime),
// overwriting any prior copy so an upgraded CLI refreshes the skill. It returns
// the destination directory.
func installReplSkill() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	destRoot := filepath.Join(home, ".claude", "skills", replSkillName)

	err = fs.WalkDir(replSkillFS, replSkillFSRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(replSkillFSRoot, p)
		if err != nil {
			return err
		}
		dest := filepath.Join(destRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, replSkillDirPerm)
		}
		data, err := replSkillFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), replSkillDirPerm); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
	if err != nil {
		return "", err
	}
	return destRoot, nil
}

// --- runtime next ---

// runtimeNextResult mirrors the daemon's runtimeNextResponse. Auth is parsed
// here but never printed — it is persisted to the job-auth file instead so the
// task token stays out of the REPL transcript.
type runtimeNextResult struct {
	Job  *runtimeJobView `json:"job"`
	Auth *jobAuth        `json:"auth,omitempty"`
}

type runtimeJobView struct {
	JobID      string `json:"job_id"`
	TaskID     string `json:"task_id,omitempty"`
	Cwd        string `json:"cwd"`
	Prompt     string `json:"prompt"`
	Model      string `json:"model,omitempty"`
	ThreadName string `json:"thread_name,omitempty"`
}

func runRuntimeNext(cmd *cobra.Command, _ []string) error {
	wait, _ := cmd.Flags().GetInt("wait")
	if wait < 0 {
		wait = 0
	}
	block, _ := cmd.Flags().GetBool("block")
	port := daemonLoopbackPort(cmd)

	// In --block mode Ctrl-C should end the loop cleanly rather than abort with
	// an error, so a user can stop the runtime gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	for {
		result, err := pollRuntimeNext(ctx, port, wait)
		if err != nil {
			if ctx.Err() != nil {
				// Interrupted (Ctrl-C): exit quietly.
				return nil
			}
			if block {
				if isPermanentNextError(err) {
					// Not a transient hiccup: the daemon is not in repl mode
					// (503) or rejected the request (4xx). Retrying would loop
					// forever and never hand the error back, so surface it and
					// let the session stop with a clear message.
					return err
				}
				// The daemon may be restarting; back off briefly and retry
				// instead of ending the session's loop on a transient error.
				fmt.Fprintf(os.Stderr, "runtime next: %v; retrying in 2s\n", err)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(2 * time.Second):
				}
				continue
			}
			return err
		}

		// With --block, keep the model asleep until there is real work: an empty
		// return re-polls internally instead of handing control back to the LLM.
		if block && result.Job == nil {
			continue
		}

		if result.Job != nil && result.Auth != nil {
			if err := writeJobAuth(result.Job.Cwd, *result.Auth); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not persist job auth (%s): %v\n", result.Job.Cwd, err)
			}
		}

		// Print only the job — never the auth secret — keeping the stable
		// {"job": ...} shape the skill consumes.
		return cli.PrintJSON(os.Stdout, map[string]any{"job": result.Job})
	}
}

// pollRuntimeNext performs a single long-poll against the local daemon.
func pollRuntimeNext(ctx context.Context, port, wait int) (*runtimeNextResult, error) {
	// Allow the daemon's long-poll window plus a margin before the HTTP client
	// gives up, so the client never times out ahead of the server's own wait.
	client := &http.Client{Timeout: time.Duration(wait)*time.Second + 10*time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/runtime/next?wait=%d", port, wait)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon on 127.0.0.1:%d (is `multica runtime repl` running?): %w", port, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("runtime next failed (%d): %s", resp.StatusCode, string(body))
		// A 503 (daemon not in repl executor mode) or any 4xx (bad request /
		// auth / unknown route) will not change on retry — it is a permanent
		// misconfiguration. Mark it so --block surfaces it and stops instead of
		// spinning forever. Other 5xx are treated as transient (a daemon that is
		// momentarily unhealthy/restarting), like a connection error.
		if resp.StatusCode == http.StatusServiceUnavailable ||
			(resp.StatusCode >= 400 && resp.StatusCode < 500) {
			return nil, &permanentNextError{err}
		}
		return nil, err
	}

	var result runtimeNextResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse daemon response: %w", err)
	}
	return &result, nil
}

// permanentNextError marks a /runtime/next failure that retrying cannot fix
// (the daemon is not in repl executor mode, or the request was rejected) — as
// opposed to a transient connection/5xx error during a daemon restart. In
// --block mode transient errors are retried, but a permanent one ends the loop
// so the failure reaches the caller instead of looping silently.
type permanentNextError struct{ err error }

func (e *permanentNextError) Error() string { return e.err.Error() }
func (e *permanentNextError) Unwrap() error { return e.err }

// isPermanentNextError reports whether err is (or wraps) a permanentNextError.
func isPermanentNextError(err error) bool {
	var p *permanentNextError
	return errors.As(err, &p)
}

// --- runtime result ---

func runRuntimeResult(cmd *cobra.Command, args []string) error {
	jobID := args[0]
	status, _ := cmd.Flags().GetString("status")
	if status != "completed" && status != "failed" {
		return fmt.Errorf("--status must be 'completed' or 'failed'")
	}
	summary, _, err := resolveTextFlag(cmd, "summary")
	if err != nil {
		return err
	}
	errDetail, _ := cmd.Flags().GetString("error")

	port := daemonLoopbackPort(cmd)
	reqBody := map[string]string{
		"job_id":  jobID,
		"status":  status,
		"summary": summary,
		"error":   errDetail,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(
		fmt.Sprintf("http://127.0.0.1:%d/runtime/result", port),
		"application/json",
		bytes.NewReader(data),
	)
	if err != nil {
		return fmt.Errorf("connect to daemon on 127.0.0.1:%d: %w", port, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	// 404 means the job is unknown (already reported / cancelled); surface the
	// daemon's JSON rather than erroring so the skill can move on cleanly.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("runtime result failed (%d): %s", resp.StatusCode, string(respBody))
	}

	// The job is over either way — drop its persisted credential so a later
	// stray `multica` call in this workdir cannot keep acting as the agent.
	removeJobAuth()

	var pretty map[string]any
	if err := json.Unmarshal(respBody, &pretty); err != nil {
		return fmt.Errorf("parse daemon response: %w", err)
	}
	return cli.PrintJSON(os.Stdout, pretty)
}

// --- runtime renew ---

func runRuntimeRenew(cmd *cobra.Command, args []string) error {
	port := daemonLoopbackPort(cmd)
	data, err := json.Marshal(map[string]string{"job_id": args[0]})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(
		fmt.Sprintf("http://127.0.0.1:%d/runtime/renew", port),
		"application/json",
		bytes.NewReader(data),
	)
	if err != nil {
		return fmt.Errorf("connect to daemon on 127.0.0.1:%d: %w", port, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	// 404 means the lease could not be renewed (job gone); surface the JSON
	// rather than erroring so the skill can react without aborting.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("runtime renew failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var pretty map[string]any
	if err := json.Unmarshal(respBody, &pretty); err != nil {
		return fmt.Errorf("parse daemon response: %w", err)
	}
	return cli.PrintJSON(os.Stdout, pretty)
}
