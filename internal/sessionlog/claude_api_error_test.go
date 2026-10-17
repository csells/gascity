package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
)

func readClaudeLines(t *testing.T, lines ...string) *Session {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	var data []byte
	for _, line := range lines {
		data = append(data, line+"\n"...)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := ReadFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

// An explicit API failure after partial assistant output stays a failure,
// and the partial reply before it stays a reply.
func TestClaudeAPIErrorAfterPartialResponse(t *testing.T) {
	partial := `{"uuid":"partial","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Partial response"}],"stop_reason":"refusal"}}`
	failure := `{"uuid":"failure","parentUuid":"partial","type":"assistant","isApiErrorMessage":true,"error":"invalid_request","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"API Error: Request blocked by provider safeguards."}],"stop_reason":"stop_sequence"}}`
	session := readClaudeLines(t, partial, failure)
	if len(session.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(session.Messages))
	}
	if reply := session.Messages[0]; reply.Type != "assistant" || reply.SystemEvent != nil {
		t.Fatalf("ordinary refusal reclassified: type %q event %+v", reply.Type, reply.SystemEvent)
	}
	got := session.Messages[1]
	want := SystemEvent{Kind: "error", Category: "provider_error", Code: "invalid_request", Message: "API Error: Request blocked by provider safeguards."}
	if got.Type != "system" || got.SystemEvent == nil || *got.SystemEvent != want {
		t.Fatalf("API error = type %q event %+v, want system %+v", got.Type, got.SystemEvent, want)
	}
	if string(got.Raw) != failure {
		t.Fatalf("raw record changed: %s", got.Raw)
	}
}

// Only the CLI's isApiErrorMessage flag marks a failure; text that merely
// reads like one is a reply.
func TestClaudeAPIErrorRequiresExplicitFlag(t *testing.T) {
	for _, flag := range []string{"false", "true"} {
		t.Run(flag, func(t *testing.T) {
			session := readClaudeLines(t, `{"uuid":"entry","type":"assistant","isApiErrorMessage":`+flag+`,"message":{"role":"assistant","content":"API Error: quoted example"}}`)
			entry := session.Messages[0]
			if flag == "false" {
				if entry.Type != "assistant" || entry.SystemEvent != nil {
					t.Fatalf("unflagged entry reclassified: type %q event %+v", entry.Type, entry.SystemEvent)
				}
				return
			}
			if entry.Type != "system" || entry.SystemEvent == nil || entry.SystemEvent.Message != "API Error: quoted example" || entry.SystemEvent.Code != "" {
				t.Fatalf("flagged entry = type %q event %+v", entry.Type, entry.SystemEvent)
			}
		})
	}
}
