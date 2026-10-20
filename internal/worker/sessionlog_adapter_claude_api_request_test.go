package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// claudeAPIRequestCaptureTranscript models a Claude Code 2.1.302 transcript
// one moment after a prompt was submitted: an earlier turn has finished, the
// new user prompt is recorded, and Claude Code has appended its api-request
// capture records (api-request-shape carrying the full system prompt and tool
// schemas, api-request-blob per message, api-request) before any assistant
// reply. None of those records defines activity, and together they exceed the
// 64 KiB tail window. The sizes and order mirror the rig transcript preserved
// from the failed BB live acceptance run (/tmp/bb-live-e761k3x_ in gcbb-test).
func claudeAPIRequestCaptureTranscript(t *testing.T) (root, path string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "-tmp-bb-work-rig")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript dir: %v", err)
	}
	path = filepath.Join(dir, "4d5903ea-542e-4f27-a18f-d48b2fd698ec.jsonl")
	const sid = `"sessionId":"4d5903ea-542e-4f27-a18f-d48b2fd698ec"`
	pad := func(n int) string { return strings.Repeat("x", n) }
	writeLines(t, path,
		// Finished first turn: the transcript was idle before the prompt.
		`{"uuid":"u1","parentUuid":null,"type":"user","message":{"role":"user","content":"start"},"timestamp":"2026-10-19T01:17:21.732Z",`+sid+`}`,
		`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ready"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}},"timestamp":"2026-10-19T01:17:24.980Z",`+sid+`}`,
		`{"uuid":"s1","parentUuid":"a1","type":"system","subtype":"turn_duration","durationMs":3000,"timestamp":"2026-10-19T01:17:25.031Z",`+sid+`}`,
		`{"type":"last-prompt","lastPrompt":"start","leafUuid":"s1",`+sid+`}`,
		// The BB prompt: a turn is now in flight.
		`{"uuid":"u2","parentUuid":"s1","type":"user","message":{"role":"user","content":"BB acceptance prompt"},"timestamp":"2026-10-19T01:17:26.925Z","promptSource":"typed",`+sid+`}`,
		`{"uuid":"t2","parentUuid":"u2","type":"attachment","attachment":{"type":"total_tokens_reminder","text":"reminder"},"timestamp":"2026-10-19T01:17:26.925Z",`+sid+`}`,
		`{"uuid":"h2","parentUuid":"t2","type":"attachment","attachment":{"type":"hook_success","hookName":"UserPromptSubmit","hookEvent":"UserPromptSubmit","exitCode":0},"timestamp":"2026-10-19T01:17:27.032Z",`+sid+`}`,
		// Claude Code's request capture, written after the prompt record.
		fmt.Sprintf(`{"type":"api-request-shape",%s,"shapeHash":"h","timestamp":"2026-10-19T01:17:27.050Z","version":"2.1.302","shape":{"system":[{"text":%q}],"tools":[]}}`, sid, pad(62*1024)),
		fmt.Sprintf(`{"type":"api-request-blob",%s,"hash":"b1","message":{"role":"user","content":[{"type":"text","text":%q}]}}`, sid, pad(3*1024)),
		fmt.Sprintf(`{"type":"api-request-blob",%s,"hash":"b2","message":{"role":"system","content":%q}}`, sid, pad(11*1024)),
		`{"type":"api-request","id":"r1","timestamp":"2026-10-19T01:17:27.050Z","version":"2.1.302","querySource":"repl_main_thread","params":{"model":"claude-opus-5-5"},`+sid+`}`,
		`{"type":"last-prompt","lastPrompt":"BB acceptance prompt","leafUuid":"h2",`+sid+`}`,
	)
	return root, path
}

// A submitted Claude prompt whose api-request capture records push it out of
// the tail window must still read as in_turn. GC reported "unknown" here, and
// the BB provider refuses any turn whose activity is unknown, so the rig scope
// of the live acceptance failed on its first turn.
func TestSessionLogAdapterLoadHistoryClaudeInTurnBehindAPIRequestCapture(t *testing.T) {
	root, path := claudeAPIRequestCaptureTranscript(t)
	adapter := SessionLogAdapter{SearchPaths: []string{root}}

	snapshot, err := adapter.LoadHistory(LoadRequest{
		Provider:       "claude",
		TranscriptPath: path,
		GCSessionID:    "ble-22",
	})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	// The full-file parse is right: it drops the capture records, and the
	// last conversational entry is the submitted prompt.
	var last *HistoryEntry
	for i := range snapshot.Entries {
		if a := snapshot.Entries[i].Actor; a == ActorUser || a == ActorAssistant {
			last = &snapshot.Entries[i]
		}
	}
	if last == nil || last.ID != "u2" || last.Actor != ActorUser {
		t.Fatalf("last conversational entry = %+v, want the submitted prompt u2", last)
	}
	if got := snapshot.TailState.Activity; got != TailActivityInTurn {
		t.Errorf("LoadHistory TailState.Activity = %q, want %q (the prompt is the last activity-defining record)", got, TailActivityInTurn)
	}

	// The worker's busy detection (handle_lifecycle.go) reads the same tail.
	activity, err := adapter.TailActivityForProvider("claude", path)
	if err != nil {
		t.Fatalf("TailActivityForProvider() error = %v", err)
	}
	if activity != TailActivityInTurn {
		t.Errorf("TailActivityForProvider = %q, want %q", activity, TailActivityInTurn)
	}
}

// Control: the global scope's transcript wrote the same capture records just
// before the prompt record, so the prompt stayed inside the tail window and the
// same turn read as in_turn. Only the write order differs.
func TestSessionLogAdapterLoadHistoryClaudeInTurnWhenCaptureRecordsPrecedePrompt(t *testing.T) {
	root, path := claudeAPIRequestCaptureTranscript(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	// Move the prompt and its attachments (lines 4-6) after the capture (7-10).
	reordered := append(append(append([]string{}, lines[:4]...), lines[7:11]...), append(lines[4:7], lines[11:]...)...)
	writeLines(t, path, reordered...)

	snapshot, err := (SessionLogAdapter{SearchPaths: []string{root}}).LoadHistory(LoadRequest{Provider: "claude", TranscriptPath: path})
	if err != nil {
		t.Fatalf("LoadHistory() error = %v", err)
	}
	if got := snapshot.TailState.Activity; got != TailActivityInTurn {
		t.Errorf("TailState.Activity = %q, want %q", got, TailActivityInTurn)
	}
}
