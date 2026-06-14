package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
)

// replTranscriptForwarder turns a REPL session's Claude Code transcript (a JSONL
// file the CLI appends to as the session runs) into the same task-message stream
// a headless subprocess reports from its stream-json output. A headless task is
// visible live in the UI because the daemon spawns the agent CLI and parses its
// output; a repl task runs inside the human's interactive session, which the
// daemon never sees. Instead, the session's PostToolUse/Stop hooks ping the
// loopback /runtime/transcript endpoint; each ping drains the lines appended
// since the last drain and forwards them, so the UI fills in between claim and
// result.
//
// Reading the transcript (rather than only PreToolUse/PostToolUse hook payloads)
// is deliberate: the transcript also contains assistant text/thinking and the
// activity of tool calls made inside spawned sub-agents, neither of which the
// per-tool hooks expose.
type replTranscriptForwarder struct {
	mu      sync.Mutex
	cursors map[string]*transcriptCursor // taskID -> cursor
}

// transcriptCursor remembers how far a task's transcript has been forwarded.
type transcriptCursor struct {
	path   string // transcript file last seen for this task
	offset int64  // bytes already forwarded
	seq    int    // next message sequence number
}

func newReplTranscriptForwarder() *replTranscriptForwarder {
	return &replTranscriptForwarder{cursors: map[string]*transcriptCursor{}}
}

// drain reads the transcript lines appended for taskID since the last call and
// returns them as task messages. firstDrain is true the first time a given
// (task, transcript path) pair is seen, so the caller can pin the session once.
// A changed path (a resumed task gets a fresh session/transcript) resets the
// cursor so the new session is forwarded from its start.
func (f *replTranscriptForwarder) drain(taskID, path string) (msgs []TaskMessageData, firstDrain bool, err error) {
	// Hold the lock across the whole drain (cursor read, file read, cursor
	// advance). Parallel tool calls fire PostToolUse hooks concurrently, so two
	// drains for the same task could otherwise read from the same offset and
	// double-report. Drains are small and infrequent (one short local-file read
	// per tool call / turn), so full serialization is cheap.
	f.mu.Lock()
	defer f.mu.Unlock()

	cur := f.cursors[taskID]
	if cur == nil || cur.path != path {
		cur = &transcriptCursor{path: path}
		f.cursors[taskID] = cur
		firstDrain = true
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, firstDrain, err
	}
	defer file.Close()

	if _, err := file.Seek(cur.offset, io.SeekStart); err != nil {
		return nil, firstDrain, err
	}

	reader := bufio.NewReader(file)
	consumed := int64(0)
	seq := cur.seq
	for {
		line, readErr := reader.ReadString('\n')
		if readErr == io.EOF {
			// A trailing line without a newline is still being written; leave it
			// for the next drain so we never forward a half-flushed JSON object.
			break
		}
		if readErr != nil {
			return nil, firstDrain, readErr
		}
		consumed += int64(len(line))
		for _, m := range parseTranscriptLine(line) {
			m.Seq = seq
			seq++
			msgs = append(msgs, m)
		}
	}

	cur.offset += consumed
	cur.seq = seq
	return msgs, firstDrain, nil
}

// forget drops a task's cursor once its job is over so a later task id reuse
// starts clean and memory does not grow unbounded.
func (f *replTranscriptForwarder) forget(taskID string) {
	f.mu.Lock()
	delete(f.cursors, taskID)
	f.mu.Unlock()
}

// transcriptLine is the subset of a Claude Code transcript JSONL record we map
// to task messages. Only `assistant` and `user` records carry activity; other
// record types (mode, system, file-history-snapshot, ...) are ignored.
type transcriptLine struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// transcriptBlock is one content block inside an assistant/user message.
type transcriptBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    map[string]any  `json:"input"`
	Content  json.RawMessage `json:"content"` // tool_result payload (string or block list)
}

// parseTranscriptLine maps one JSONL line to zero or more task messages. It
// never errors on a malformed line — a transcript is best-effort telemetry, so a
// line we cannot parse is skipped rather than aborting the whole drain.
func parseTranscriptLine(line string) []TaskMessageData {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	var rec transcriptLine
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return nil
	}
	if rec.Type != "assistant" && rec.Type != "user" {
		return nil
	}
	if len(rec.Message.Content) == 0 {
		return nil
	}

	// Content is usually a block list; a bare string is a plain user prompt,
	// which we do not forward (the UI stream mirrors agent output, not input).
	var blocks []transcriptBlock
	if err := json.Unmarshal(rec.Message.Content, &blocks); err != nil {
		return nil
	}

	var out []TaskMessageData
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if rec.Type == "assistant" && strings.TrimSpace(b.Text) != "" {
				out = append(out, TaskMessageData{Type: "text", Content: b.Text})
			}
		case "thinking":
			if strings.TrimSpace(b.Thinking) != "" {
				out = append(out, TaskMessageData{Type: "thinking", Content: b.Thinking})
			}
		case "tool_use":
			out = append(out, TaskMessageData{Type: "tool_use", Tool: b.Name, Input: b.Input})
		case "tool_result":
			out = append(out, TaskMessageData{Type: "tool_result", Output: rawToolResult(b.Content)})
		}
	}
	return out
}

// rawToolResult renders a tool_result payload, which is either a JSON string or
// a list of content blocks (each typically {type:"text", text:"..."}).
func rawToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []transcriptBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Text != "" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return string(raw)
}
