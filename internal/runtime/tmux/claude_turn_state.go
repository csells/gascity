package tmux

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

type claudeOwnedTask struct {
	ID, ToolID, TurnID, Command string
	Done                        bool
}

type claudeTurnState struct {
	TurnID, Version string
	Tasks           map[string]claudeOwnedTask
	Pending         map[string]string // tool ID -> owning turn
	Unsupported     map[string]string // owning turn -> native tool
	Boundaries      map[string]bool   // native interrupt or completed-turn record
}

// readClaudeTurnState consumes only the keyed native transcript. Tool results
// and interruption messages never establish a new user turn. Backgrounding is
// not detachment, and a terminal notification must match both native IDs.
func readClaudeTurnState(r io.Reader, sessionKey string) (claudeTurnState, error) {
	s := claudeTurnState{Tasks: map[string]claudeOwnedTask{}, Pending: map[string]string{}, Unsupported: map[string]string{}, Boundaries: map[string]bool{}}
	owners := map[string]string{}
	tools := map[string]claudeOwnedTask{}
	type notification struct {
		XMLName xml.Name `xml:"task-notification"`
		ID      string   `xml:"task-id"`
		ToolID  string   `xml:"tool-use-id"`
		Status  string   `xml:"status"`
	}
	var notifications []notification
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 256*1024), 50*1024*1024)
	for scan.Scan() {
		if len(strings.TrimSpace(scan.Text())) == 0 {
			continue
		}
		var row struct {
			Type, Subtype, UUID, ParentUUID, SessionID, PromptID, InterruptedMessageID, Operation, Content, Version string
			IsSidechain, IsMeta, IsCompactSummary                                                                   bool
			Message                                                                                                 struct{ Content json.RawMessage }
			ToolUseResult                                                                                           json.RawMessage
		}
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			return s, fmt.Errorf("incomplete native Claude transcript: %w", err)
		}
		if (row.SessionID != "" || row.Type == "user" || row.Type == "assistant" || row.Type == "queue-operation") && row.SessionID != sessionKey {
			return s, fmt.Errorf("native Claude transcript identity mismatch")
		}
		if row.IsSidechain {
			continue
		}
		if row.Version != "" {
			s.Version = row.Version
		}
		if row.Type == "queue-operation" && row.Operation == "enqueue" && strings.HasPrefix(row.Content, "<task-notification>") {
			var n notification
			if err := xml.Unmarshal([]byte(row.Content), &n); err != nil {
				return s, fmt.Errorf("invalid native task completion: %w", err)
			}
			notifications = append(notifications, n)
			continue
		}
		var blocks []struct {
			Type, ID, Name string
			ToolUseID      string `json:"tool_use_id"`
			Input          struct{ Command string }
		}
		var plain string
		if len(row.Message.Content) != 0 {
			if err := json.Unmarshal(row.Message.Content, &blocks); err != nil {
				if err := json.Unmarshal(row.Message.Content, &plain); err != nil {
					return s, fmt.Errorf("invalid native message content: %w", err)
				}
			}
		}
		owner := owners[row.ParentUUID]
		isToolResult := false
		for _, b := range blocks {
			if b.Type == "tool_result" {
				isToolResult = true
			}
		}
		if row.Type == "user" && row.PromptID != "" && !row.IsMeta && !row.IsCompactSummary && row.InterruptedMessageID == "" && !isToolResult {
			owner = row.PromptID
			s.TurnID = owner
		}
		if row.UUID != "" {
			owners[row.UUID] = owner
		}
		if owner != "" && (row.InterruptedMessageID != "" || (row.Type == "system" && row.Subtype == "turn_duration")) {
			s.Boundaries[owner] = true
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				if owner == "" {
					return s, fmt.Errorf("native tool has no established user-turn owner")
				}
				if b.Name != "Bash" {
					// These built-ins do not own background shell execution.
					switch b.Name {
					case "Read", "Write", "Edit", "MultiEdit", "Glob", "Grep", "WebFetch", "WebSearch", "TodoWrite", "TaskCreate", "TaskUpdate", "TaskList", "TaskGet", "TaskOutput", "TaskStop", "AskUserQuestion", "EnterPlanMode", "ExitPlanMode", "NotebookEdit":
					default:
						s.Unsupported[owner] = b.Name
					}
					continue
				}
				if b.ID == "" || strings.TrimSpace(b.Input.Command) == "" {
					return s, fmt.Errorf("incomplete native Bash identity")
				}
				tools[b.ID] = claudeOwnedTask{ToolID: b.ID, TurnID: owner, Command: b.Input.Command}
				s.Pending[b.ID] = owner
			case "tool_result":
				task, ok := tools[b.ToolUseID]
				if !ok {
					continue
				}
				if owner != task.TurnID {
					return s, fmt.Errorf("native command result crossed user turns")
				}
				var result struct {
					BackgroundTaskID string
					Interrupted      *bool
					Stdout           *string
				}
				if err := json.Unmarshal(row.ToolUseResult, &result); err != nil {
					// Claude also synthesizes "User rejected tool use" after
					// interrupting an already-started command. That text is not
					// proof that the process exited: retain the pending barrier.
					var rejection string
					if json.Unmarshal(row.ToolUseResult, &rejection) == nil {
						continue
					}
					return s, fmt.Errorf("unconfirmed native Bash result: %w", err)
				}
				if result.BackgroundTaskID != "" {
					task.ID = result.BackgroundTaskID
					if previous, exists := s.Tasks[task.ID]; exists && previous.ToolID != task.ToolID {
						return s, fmt.Errorf("native task ID reused")
					}
					s.Tasks[task.ID] = task
				} else if result.Interrupted == nil && result.Stdout == nil {
					return s, fmt.Errorf("native command result did not confirm foreground completion")
				}
				delete(s.Pending, b.ToolUseID)
			}
		}
	}
	if err := scan.Err(); err != nil {
		return s, fmt.Errorf("reading native command ownership: %w", err)
	}
	for _, n := range notifications {
		task, ok := s.Tasks[n.ID]
		if !ok {
			continue
		}
		if task.ToolID != n.ToolID {
			return s, fmt.Errorf("native task completion identity mismatch")
		}
		switch n.Status {
		case "killed", "completed", "failed":
			task.Done = true
			s.Tasks[n.ID] = task
		default:
			return s, fmt.Errorf("unknown native task completion state %q", n.Status)
		}
	}
	return s, nil
}
