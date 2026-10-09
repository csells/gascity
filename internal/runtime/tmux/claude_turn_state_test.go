package tmux

import (
	"strings"
	"testing"
)

func TestClaudeOwnedTaskEvidence(t *testing.T) {
	pane := "old chat Command: wanted\n   Shell details\n\n Status: running\n Command: other\n          command\n Output:\n"
	if got := claudeTaskDetailCommand(pane); got != "othercommand" {
		t.Fatalf("task selector borrowed chat evidence: %q", got)
	}
	const transcript = `{"type":"user","uuid":"u","sessionId":"conversation","promptId":"turn","message":{"content":"work"}}
{"type":"assistant","uuid":"a","parentUuid":"u","sessionId":"conversation","message":{"content":[{"type":"tool_use","id":"tool","name":"Bash","input":{"command":"sleep 300","run_in_background":true}}]}}
{"type":"user","uuid":"r","parentUuid":"a","sessionId":"conversation","message":{"content":[{"type":"tool_result","tool_use_id":"tool"}]},"toolUseResult":{"backgroundTaskId":"task123"}}
{"type":"user","uuid":"i","parentUuid":"r","sessionId":"conversation","promptId":"turn","interruptedMessageId":"message","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}
`
	s, err := readClaudeTurnState(strings.NewReader(transcript), "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if s.TurnID != "turn" || len(s.Tasks) != 1 || s.Tasks["task123"].Done || len(s.Pending) != 0 {
		t.Fatalf("background return or interrupt lost command ownership: %+v", s)
	}
	// A completion belonging to a different tool cannot confirm this command.
	wrong := `{"type":"queue-operation","operation":"enqueue","sessionId":"conversation","content":"<task-notification><task-id>task123</task-id><tool-use-id>other</tool-use-id><status>killed</status></task-notification>"}`
	s, err = readClaudeTurnState(strings.NewReader(transcript+wrong), "conversation")
	if err == nil && s.Tasks["task123"].Done {
		t.Fatal("accepted unrelated native completion")
	}
	right := strings.ReplaceAll(wrong, ">other<", ">tool<")
	s, err = readClaudeTurnState(strings.NewReader(transcript+right), "conversation")
	if err != nil || !s.Tasks["task123"].Done {
		t.Fatalf("exact native kill unconfirmed: %+v, %v", s, err)
	}
	// Native notifications can precede the persisted tool result. Correlate
	// after the complete read rather than losing that completion forever.
	s, err = readClaudeTurnState(strings.NewReader(right+"\n"+transcript), "conversation")
	if err != nil || !s.Tasks["task123"].Done {
		t.Fatalf("early completion lost: %+v %v", s, err)
	}
}

func TestClaudeOwnedTaskStartBarrier(t *testing.T) {
	const input = `{"type":"user","uuid":"u","sessionId":"s","promptId":"t","message":{"content":"work"}}
{"type":"assistant","uuid":"a","parentUuid":"u","sessionId":"s","message":{"content":[{"type":"tool_use","id":"tool","name":"Bash","input":{"command":"sleep 300"}}]}}
`
	s, err := readClaudeTurnState(strings.NewReader(input), "s")
	if err != nil || len(s.Pending) != 1 {
		t.Fatalf("unretained command start must block success: %+v %v", s, err)
	}
	rejected := input + `{"type":"user","uuid":"r","parentUuid":"a","sessionId":"s","message":{"content":[{"type":"tool_result","tool_use_id":"tool"}]},"toolUseResult":"User rejected tool use"}`
	s, err = readClaudeTurnState(strings.NewReader(rejected), "s")
	if err != nil || len(s.Pending) != 1 {
		t.Fatalf("generic rejection incorrectly confirmed command exit: %+v %v", s, err)
	}
	for _, bad := range []string{input + `{"type":`, strings.ReplaceAll(input, `"sessionId":"s"`, `"sessionId":"other"`)} {
		if _, err := readClaudeTurnState(strings.NewReader(bad), "s"); err == nil {
			t.Fatal("accepted incomplete/foreign transcript")
		}
	}
	s, err = readClaudeTurnState(strings.NewReader(strings.ReplaceAll(input, `"name":"Bash"`, `"name":"Agent"`)), "s")
	if err != nil || s.Unsupported["t"] != "Agent" {
		t.Fatalf("unverified descendant ownership ignored: %+v %v", s, err)
	}
}

func TestClaudeOwnedTasksStayWithTheirTurn(t *testing.T) {
	const input = `{"type":"user","uuid":"u","sessionId":"s","promptId":"old","message":{"content":"work"}}
{"type":"assistant","uuid":"a","parentUuid":"u","sessionId":"s","message":{"content":[{"type":"tool_use","id":"tool","name":"Bash","input":{"command":"sleep 300"}}]}}
{"type":"user","uuid":"r","parentUuid":"a","sessionId":"s","message":{"content":[{"type":"tool_result","tool_use_id":"tool"}]},"toolUseResult":{"backgroundTaskId":"task123"}}
{"type":"user","uuid":"u2","parentUuid":"r","sessionId":"s","promptId":"new","message":{"content":"new work"}}
`
	s, err := readClaudeTurnState(strings.NewReader(input), "s")
	if err != nil || s.TurnID != "new" || s.Tasks["task123"].TurnID != "old" {
		t.Fatalf("lost turn fence: %+v %v", s, err)
	}
}
