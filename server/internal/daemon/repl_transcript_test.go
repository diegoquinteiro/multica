package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTranscriptLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []TaskMessageData
	}{
		{
			name: "assistant text",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`,
			want: []TaskMessageData{{Type: "text", Content: "hello"}},
		},
		{
			name: "assistant thinking",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}}`,
			want: []TaskMessageData{{Type: "thinking", Content: "hmm"}},
		},
		{
			name: "assistant tool_use",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`,
			want: []TaskMessageData{{Type: "tool_use", Tool: "Bash", Input: map[string]any{"command": "ls"}}},
		},
		{
			name: "user tool_result string",
			line: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"done"}]}}`,
			want: []TaskMessageData{{Type: "tool_result", Output: "done"}},
		},
		{
			name: "user tool_result block list",
			line: `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"out"}]}]}}`,
			want: []TaskMessageData{{Type: "tool_result", Output: "out"}},
		},
		{
			name: "user plain prompt string is skipped",
			line: `{"type":"user","message":{"role":"user","content":"hi there"}}`,
			want: nil,
		},
		{
			name: "non assistant/user type skipped",
			line: `{"type":"system","subtype":"x"}`,
			want: nil,
		},
		{
			name: "malformed json skipped",
			line: `{not json`,
			want: nil,
		},
		{
			name: "empty assistant text skipped",
			line: `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"  "}]}}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTranscriptLine(tc.line)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d messages, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i].Type != tc.want[i].Type || got[i].Tool != tc.want[i].Tool ||
					got[i].Content != tc.want[i].Content || got[i].Output != tc.want[i].Output {
					t.Fatalf("msg %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestDrainIsIncremental verifies a drain only returns lines appended since the
// previous drain, advances the byte offset, assigns increasing sequences, and
// ignores a trailing partial (newline-less) line until it is completed.
func TestDrainIsIncremental(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	f := newReplTranscriptForwarder()
	taskID := "task-1"

	write := func(s string) {
		t.Helper()
		fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fh.WriteString(s); err != nil {
			t.Fatal(err)
		}
		fh.Close()
	}

	write(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"one"}]}}` + "\n")
	msgs, first, err := f.drain(taskID, path)
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first drain should report firstDrain=true")
	}
	if len(msgs) != 1 || msgs[0].Content != "one" || msgs[0].Seq != 0 {
		t.Fatalf("drain 1 = %+v", msgs)
	}

	// Append a complete line plus a partial (no trailing newline yet).
	write(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"two"}]}}` + "\n")
	write(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial"`)
	msgs, first, err = f.drain(taskID, path)
	if err != nil {
		t.Fatal(err)
	}
	if first {
		t.Fatal("second drain should report firstDrain=false")
	}
	if len(msgs) != 1 || msgs[0].Content != "two" || msgs[0].Seq != 1 {
		t.Fatalf("drain 2 = %+v (partial line must not be forwarded, seq must continue)", msgs)
	}

	// Complete the partial line; it should now be forwarded once.
	write(`}]}}` + "\n")
	msgs, _, err = f.drain(taskID, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "partial" || msgs[0].Seq != 2 {
		t.Fatalf("drain 3 = %+v", msgs)
	}

	// No new content → nothing.
	msgs, _, err = f.drain(taskID, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("drain 4 should be empty, got %+v", msgs)
	}
}

// TestDrainResetsOnNewTranscript verifies that a changed transcript path (a
// resumed task with a fresh session) restarts forwarding from the new file.
func TestDrainResetsOnNewTranscript(t *testing.T) {
	dir := t.TempDir()
	f := newReplTranscriptForwarder()
	taskID := "task-1"

	p1 := filepath.Join(dir, "a.jsonl")
	os.WriteFile(p1, []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"a"}]}}`+"\n"), 0o644)
	if msgs, first, _ := f.drain(taskID, p1); len(msgs) != 1 || !first {
		t.Fatalf("p1 drain = %+v first=%v", msgs, first)
	}

	p2 := filepath.Join(dir, "b.jsonl")
	os.WriteFile(p2, []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"b"}]}}`+"\n"), 0o644)
	msgs, first, _ := f.drain(taskID, p2)
	if len(msgs) != 1 || msgs[0].Content != "b" || !first || msgs[0].Seq != 0 {
		t.Fatalf("p2 drain = %+v first=%v (new path must reset cursor)", msgs, first)
	}
}
