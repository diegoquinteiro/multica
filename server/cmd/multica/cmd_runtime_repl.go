package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
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

func init() {
	runtimeCmd.AddCommand(runtimeReplCmd)
	runtimeCmd.AddCommand(runtimeNextCmd)
	runtimeCmd.AddCommand(runtimeResultCmd)

	runtimeReplCmd.Flags().Bool("skip-skill-install", false, "Do not (re)install the multica-repl-runtime skill into ~/.claude/skills")

	runtimeNextCmd.Flags().Int("wait", 25, "Seconds to long-poll for a task before returning {\"job\": null} (max 60)")
	runtimeNextCmd.Flags().String("output", "json", "Output format: json")

	runtimeResultCmd.Flags().String("status", "completed", "Task outcome: completed or failed")
	runtimeResultCmd.Flags().String("summary", "", "Short outcome summary (decodes \\n, \\r, \\t, \\\\)")
	runtimeResultCmd.Flags().Bool("summary-stdin", false, "Read the summary from stdin (verbatim, multi-line)")
	runtimeResultCmd.Flags().String("summary-file", "", "Read the summary from a UTF-8 file (verbatim, multi-line)")
	runtimeResultCmd.Flags().String("error", "", "Error detail when --status failed")
	runtimeResultCmd.Flags().String("output", "json", "Output format: json")
}

// daemonLoopbackPort resolves the local daemon's loopback port: an explicit
// MULTICA_DAEMON_PORT (set inside daemon-spawned tasks) wins, otherwise the
// health port for the active profile — the same port `daemon status` probes.
func daemonLoopbackPort(cmd *cobra.Command) int {
	if v := os.Getenv("MULTICA_DAEMON_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return healthPortForProfile(resolveProfile(cmd))
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

func runRuntimeNext(cmd *cobra.Command, _ []string) error {
	wait, _ := cmd.Flags().GetInt("wait")
	if wait < 0 {
		wait = 0
	}
	port := daemonLoopbackPort(cmd)

	// Allow the daemon's long-poll window plus a margin before the HTTP client
	// gives up, so the client never times out ahead of the server's own wait.
	client := &http.Client{Timeout: time.Duration(wait)*time.Second + 10*time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/runtime/next?wait=%d", port, wait)
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("connect to daemon on 127.0.0.1:%d (is `multica runtime repl` running?): %w", port, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime next failed (%d): %s", resp.StatusCode, string(body))
	}

	// Pass the daemon's JSON through unchanged so the skill sees a stable shape.
	var pretty map[string]any
	if err := json.Unmarshal(body, &pretty); err != nil {
		return fmt.Errorf("parse daemon response: %w", err)
	}
	return cli.PrintJSON(os.Stdout, pretty)
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

	var pretty map[string]any
	if err := json.Unmarshal(respBody, &pretty); err != nil {
		return fmt.Errorf("parse daemon response: %w", err)
	}
	return cli.PrintJSON(os.Stdout, pretty)
}
