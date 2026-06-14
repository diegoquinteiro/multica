package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

// writeTestJobAuth seeds a job-auth file under dir/.multica/job-auth.json.
func writeTestJobAuth(t *testing.T, dir string, ja jobAuth) {
	t.Helper()
	if err := writeJobAuth(dir, ja); err != nil {
		t.Fatalf("writeJobAuth: %v", err)
	}
}

func TestJobAuthRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := jobAuth{
		Token:       "mat_test",
		AgentID:     "agent-1",
		TaskID:      "task-1",
		WorkspaceID: "ws-1",
		AgentName:   "Programador",
		DaemonPort:  19514,
	}
	writeTestJobAuth(t, dir, want)

	// Reading from the job's cwd resolves the credential.
	t.Chdir(dir)
	if got := loadJobAuth(); got != want {
		t.Fatalf("loadJobAuth() = %+v, want %+v", got, want)
	}

	// The file mode must not be world/group readable — it holds a token.
	info, err := os.Stat(filepath.Join(dir, jobAuthRelPath))
	if err != nil {
		t.Fatalf("stat job-auth: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("job-auth perm = %o, want 600", perm)
	}
}

func TestFindJobAuthFileWalksUp(t *testing.T) {
	root := t.TempDir()
	writeTestJobAuth(t, root, jobAuth{Token: "mat_root"})

	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(sub)

	if got := loadJobAuth().Token; got != "mat_root" {
		t.Fatalf("loadJobAuth().Token = %q, want walked-up %q", got, "mat_root")
	}
}

func TestResolveTokenJobAuthFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no profile token
	dir := t.TempDir()
	writeTestJobAuth(t, dir, jobAuth{Token: "mat_job"})
	t.Chdir(dir)

	t.Run("falls back to job-auth when no env or profile", func(t *testing.T) {
		t.Setenv("MULTICA_TOKEN", "")
		if got := resolveToken(testCmd()); got != "mat_job" {
			t.Fatalf("resolveToken() = %q, want %q (job-auth)", got, "mat_job")
		}
	})

	t.Run("env token still wins over job-auth", func(t *testing.T) {
		t.Setenv("MULTICA_TOKEN", "mat_env")
		if got := resolveToken(testCmd()); got != "mat_env" {
			t.Fatalf("resolveToken() = %q, want %q (env precedence)", got, "mat_env")
		}
	})
}

func TestResolveWorkspaceIDJobAuthFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := cli.SaveCLIConfig(cli.CLIConfig{WorkspaceID: "config-ws"}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	t.Setenv("MULTICA_AGENT_ID", "")
	t.Setenv("MULTICA_TASK_ID", "")
	t.Setenv("MULTICA_WORKSPACE_ID", "")

	dir := t.TempDir()
	writeTestJobAuth(t, dir, jobAuth{WorkspaceID: "job-ws", AgentID: "agent-1", TaskID: "task-1"})
	t.Chdir(dir)

	// A repl job's workspace comes from its credential, never the user config.
	if got := resolveWorkspaceID(testCmd()); got != "job-ws" {
		t.Fatalf("resolveWorkspaceID() = %q, want %q (job-auth over config)", got, "job-ws")
	}
	// The job is also recognized as an agent execution context.
	if !inAgentExecutionContext() {
		t.Fatal("inAgentExecutionContext() = false, want true inside a repl job")
	}
}

func TestRemoveJobAuth(t *testing.T) {
	dir := t.TempDir()
	writeTestJobAuth(t, dir, jobAuth{Token: "mat_x"})
	t.Chdir(dir)

	removeJobAuth()
	if _, err := os.Stat(filepath.Join(dir, jobAuthRelPath)); !os.IsNotExist(err) {
		t.Fatalf("job-auth still present after removeJobAuth: err=%v", err)
	}
	// loadJobAuth on a cleaned dir is a safe zero value.
	if got := loadJobAuth(); got != (jobAuth{}) {
		t.Fatalf("loadJobAuth() after remove = %+v, want zero", got)
	}
}
