package sessionlog

import (
	"encoding/json"
	"testing"
	"time"
)

// TestBuildDagKeepsParallelCallsFromOneResponse pins the layout Claude Code
// 2.1.296 writes for parallel tool calls (observed live): one API response is
// split into rows chained by parentUuid and sharing message.id (thinking ->
// call-a -> call-b). Each result is parented to its own call, and the reply
// continues from the EARLIER call's result. The selected branch then runs
// through call-a and result-a, leaving call-b and result-b on a sibling
// branch. Both calls and both results belong to the conversation.
func TestBuildDagKeepsParallelCallsFromOneResponse(t *testing.T) {
	msg := func(role, id, content string) json.RawMessage {
		return json.RawMessage(`{"role":"` + role + `","id":"` + id + `","content":` + content + `}`)
	}
	entries := []*Entry{
		{UUID: "root", Type: "user", Message: msg("user", "", `"run both"`)},
		{UUID: "think", ParentUUID: "root", Type: "assistant", Message: msg("assistant", "msg_1", `[{"type":"thinking","thinking":""}]`)},
		{UUID: "call-a", ParentUUID: "think", Type: "assistant", Message: msg("assistant", "msg_1", `[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"echo PAR1"}}]`)},
		{UUID: "call-b", ParentUUID: "call-a", Type: "assistant", Message: msg("assistant", "msg_1", `[{"type":"tool_use","id":"b","name":"Bash","input":{"command":"echo PAR2"}}]`)},
		{UUID: "result-b", ParentUUID: "call-b", Type: "user", Message: msg("user", "", `[{"type":"tool_result","tool_use_id":"b","content":"PAR2"}]`)},
		{UUID: "result-a", ParentUUID: "call-a", Type: "user", Message: msg("user", "", `[{"type":"tool_result","tool_use_id":"a","content":"PAR1"}]`)},
		{UUID: "answer", ParentUUID: "result-a", Type: "assistant", Message: msg("assistant", "msg_2", `[{"type":"text","text":"done"}]`)},
	}
	for i, e := range entries {
		e.Timestamp = time.Unix(int64(i), 0)
	}
	dag := BuildDag(entries)
	var got []string
	for _, e := range dag.ActiveBranch {
		got = append(got, e.UUID)
	}
	want := []string{"root", "think", "call-a", "call-b", "result-b", "result-a", "answer"}
	if len(got) != len(want) {
		t.Fatalf("active branch = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("active branch = %v, want %v", got, want)
		}
	}
	if len(dag.OrphanedToolUseIDs) != 0 {
		t.Fatalf("answered parallel calls reported open: %v", dag.OrphanedToolUseIDs)
	}

	// A sibling assistant row from a different response is another branch,
	// not part of the selected conversation.
	other := &Entry{
		UUID: "other", ParentUUID: "call-a", Type: "assistant", Timestamp: time.Unix(1, 0),
		Message: msg("assistant", "msg_other", `[{"type":"tool_use","id":"x","name":"Bash","input":{"command":"echo nope"}}]`),
	}
	dag = BuildDag(append(append([]*Entry{}, entries...), other))
	for _, e := range dag.ActiveBranch {
		if e.UUID == "other" {
			t.Fatal("a sibling row from another response entered the selected conversation")
		}
	}
}
