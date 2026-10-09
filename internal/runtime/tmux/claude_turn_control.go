package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var claudeTaskID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,100}$`)

// controlClaudeTurn uses Claude's local, non-inference task control. Pane
// contents can establish which native control is open, never termination.
// Success requires the exact native task/tool completion in the keyed log.
// Both the session mutation lock (caller) and nudge lock fence GC submissions.
func (p *Provider) controlClaudeTurn(ctx context.Context, name string, req runtime.TurnControlRequest) (runtime.TurnTarget, error) {
	target := runtime.TurnTarget{Mode: "owned_commands", SessionKey: req.SessionKey, State: "unconfirmed"}
	if req.Action != "target" && req.Action != "stop" && req.Action != "detach" {
		return target, fmt.Errorf("unsupported Claude command control action")
	}
	if req.Action == "stop" && req.TurnID != "" && req.Stopped[req.TurnID] {
		target.TurnID, target.State = req.TurnID, "stopped"
		return target, nil
	}
	if req.SessionKey == "" || req.TranscriptPath == "" {
		if req.Action == "target" {
			target.State = "initializing"
			return target, nil
		}
		return target, fmt.Errorf("%w: exact native Claude transcript unavailable", runtime.ErrInteractionUnsupported)
	}
	read := func() (claudeTurnState, error) {
		f, err := os.Open(req.TranscriptPath)
		if err != nil {
			return claudeTurnState{}, err
		}
		s, readErr := readClaudeTurnState(f, req.SessionKey)
		closeErr := f.Close()
		return s, errors.Join(readErr, closeErr)
	}
	if !acquireNudgeLock(name, p.tm.cfg.NudgeLockTimeout) {
		return target, fmt.Errorf("native command control nudge lock timeout")
	}
	defer releaseNudgeLock(name)
	s, err := read()
	if err != nil {
		return target, err
	}
	target.TurnID = s.TurnID
	if s.TurnID == "" {
		target.State = "idle"
	} else {
		target.State = "active"
	}
	if req.Stopped[s.TurnID] {
		target.State = "stopped"
	}
	if s.Boundaries[s.TurnID] {
		for _, owner := range s.Pending {
			if owner == s.TurnID {
				target.State = "unconfirmed"
			}
		}
	}
	if req.Action == "target" {
		return target, nil
	}
	if req.TurnID == "" || req.TurnID != s.TurnID {
		return target, fmt.Errorf("stale Claude turn target; no control was sent")
	}
	if s.Version != "2.1.292" {
		return target, fmt.Errorf("%w: native Claude task control is unverified for version %q", runtime.ErrInteractionUnsupported, s.Version)
	}
	if req.Action == "detach" {
		task, exists := s.Tasks[req.ProcessID]
		if !exists || task.TurnID != req.TurnID || task.Done {
			return target, fmt.Errorf("native command is not live work owned by this turn")
		}
		return target, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := p.tm.noClientAttached(name); err != nil {
		return target, err
	}
	idle, err := p.tm.SnapshotIdle(name)
	if err != nil {
		return target, err
	}
	if !idle {
		// Retain an exact native task handle before interrupting a running
		// foreground Bash. Claude's generic rejected-tool interrupt result
		// otherwise discards the handle and cannot confirm process exit.
		pending := false
		for _, owner := range s.Pending {
			pending = pending || owner == req.TurnID
		}
		if pending {
			deadline := time.Now().Add(5 * time.Second)
			for pending {
				if err := p.tm.noClientAttached(name); err != nil {
					return target, err
				}
				s, err = read()
				if err != nil {
					return target, err
				}
				if s.TurnID != req.TurnID {
					return target, fmt.Errorf("native turn changed before command handle retention")
				}
				pending = false
				for _, owner := range s.Pending {
					pending = pending || owner == req.TurnID
				}
				if !pending {
					break
				}
				if time.Now().After(deadline) {
					return target, fmt.Errorf("native foreground command handle unavailable; termination unconfirmed")
				}
				pane, err := p.tm.CapturePane(name, 80)
				if err != nil {
					return target, err
				}
				// The process can start before Claude mounts its key handler.
				// Wait for the native affordance and retry only this idempotent
				// local control while the same turn still owns an unretained start.
				if strings.Contains(pane, "to run in background") {
					if strings.Contains(pane, "ctrl+x") {
						if err := p.sendClaudeControlKey(name, "C-x"); err != nil {
							return target, err
						}
					}
					if err := p.sendClaudeControlKey(name, "C-b"); err != nil {
						return target, err
					}
				}
				if err := sleepWithContext(ctx, 150*time.Millisecond); err != nil {
					return target, err
				}
			}

			idle, err = p.tm.SnapshotIdle(name)
			if err != nil {
				return target, err
			}
		}
	}
	if !idle || !s.Boundaries[req.TurnID] {
		if err := p.sendClaudeControlKey(name, "C-c"); err != nil {
			return target, err
		}
		if err := p.tm.WaitForIdle(ctx, name, 10*time.Second); err != nil {
			return target, err
		}
		// Claude can restore the interrupted prompt. Only our detached native
		// runtime may have that restored input cleared; attached drafts survive.
		in, ok, err := p.tm.readInput(name)
		if err != nil || !ok {
			return target, fmt.Errorf("native input after interruption is unconfirmed: %w", errors.Join(err, runtime.ErrInteractionUnsupported))
		}
		if !in.empty() {
			if err := p.tm.emptyInput(name, in, func() error { return p.tm.noClientAttached(name) }); err != nil {
				return target, err
			}
		}
	}
	// The native interrupt/completion boundary plus resolved Bash results is
	// the start barrier. An empty task list alone cannot satisfy it.
	for {
		s, err = read()
		if err != nil {
			return target, err
		}
		if s.TurnID != req.TurnID {
			return target, fmt.Errorf("native turn changed during command cancellation")
		}
		if tool := s.Unsupported[req.TurnID]; tool != "" {
			return target, fmt.Errorf("%w: command ownership for native tool %q", runtime.ErrInteractionUnsupported, tool)
		}
		pending := false
		for _, owner := range s.Pending {
			pending = pending || owner == req.TurnID
		}
		if s.Boundaries[req.TurnID] && !pending {
			break
		}
		if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
			return target, fmt.Errorf("native command start barrier unconfirmed: %w", err)
		}
	}
	var ids []string
	for id, task := range s.Tasks {
		if task.TurnID == req.TurnID && !task.Done && !req.Detached[req.TurnID+":"+id] {
			ids = append(ids, id)
		}
	}
	// The native list omits IDs from its rows. A truncated command must be
	// unique across every live shell before a detail view can identify it.
	for _, id := range ids {
		prefix := claudeCommandPrefix(s.Tasks[id].Command)
		for otherID, other := range s.Tasks {
			if otherID != id && !other.Done && claudeCommandPrefix(other.Command) == prefix {
				return target, fmt.Errorf("native task controls cannot distinguish identical live commands")
			}
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := p.stopClaudeTask(ctx, name, s.Tasks[id], read); err != nil {
			return target, err
		}
	}
	s, err = read()
	if err != nil {
		return target, err
	}
	if s.TurnID != req.TurnID {
		return target, fmt.Errorf("native turn changed before command completion confirmation")
	}
	for _, task := range s.Tasks {
		if task.TurnID == req.TurnID && !task.Done && !req.Detached[req.TurnID+":"+task.ID] {
			return target, fmt.Errorf("native command termination unconfirmed")
		}
	}
	target.State = "stopped"
	return target, nil
}

func (p *Provider) stopClaudeTask(ctx context.Context, name string, task claudeOwnedTask, read func() (claudeTurnState, error)) (resultErr error) {
	if !claudeTaskID.MatchString(task.ID) || claudeCommandPrefix(task.Command) == "" {
		return fmt.Errorf("invalid native task identity")
	}
	if err := p.tm.noClientAttached(name); err != nil {
		return err
	}
	in, ok, err := p.tm.readInput(name)
	if err != nil {
		return err
	}
	if !ok || !in.empty() {
		return fmt.Errorf("native task control requires an empty idle composer")
	}
	// This is a native local command, never a model prompt. Its arguments
	// do not reliably select a task; the unique native command and its exact
	// completion IDs provide identity. Failures cannot select another owner.
	if _, err := p.tm.run("send-keys", "-t", paneTarget(name), "-l", "/tasks"); err != nil {
		return err
	}
	if err := sleepWithContext(ctx, time.Duration(p.tm.cfg.DebounceMs)*time.Millisecond); err != nil {
		return err
	}
	if err := p.sendClaudeControlKey(name, "Enter"); err != nil {
		return err
	}
	open := true
	visited := map[string]bool{}
	defer func() {
		if !open {
			return
		}
		if err := p.tm.noClientAttached(name); err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		pane, err := p.tm.CapturePane(name, 80)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
			return
		}
		if strings.Contains(pane, "Shell details") || strings.Contains(pane, "No tasks currently running") {
			resultErr = errors.Join(resultErr, p.sendClaudeControlKey(name, "Escape"))
		}
	}()
	for {
		if err := p.tm.noClientAttached(name); err != nil {
			return err
		}
		s, err := read()
		if err != nil {
			return err
		}
		if s.TurnID != task.TurnID {
			return fmt.Errorf("native turn changed before task stop")
		}
		if current, exists := s.Tasks[task.ID]; exists && current.Done {
			break
		}
		pane, err := p.tm.CapturePane(name, 80)
		if err != nil {
			return err
		}
		if strings.Contains(pane, "Shell details") && strings.Contains(pane, "x to stop") {
			// Claude wraps the command in the pane and truncates it after 280
			// characters. Confirm the uniquely identified shell; never key an
			// unrelated task picker.
			compactPane := claudeTaskDetailCommand(pane)
			if !strings.HasPrefix(compactPane, claudeCommandPrefix(task.Command)) {
				if visited[compactPane] {
					return fmt.Errorf("native task picker did not identify the requested command")
				}
				visited[compactPane] = true
				if err := p.sendClaudeControlKey(name, "Left"); err != nil {
					return err
				}
				if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
					return err
				}
				if err := p.sendClaudeControlKey(name, "Down"); err != nil {
					return err
				}
				if err := p.sendClaudeControlKey(name, "Enter"); err != nil {
					return err
				}
				if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			if err := p.tm.noClientAttached(name); err != nil {
				return err
			}
			if err := p.sendClaudeControlKey(name, "x"); err != nil {
				return err
			}
			break
		}
		if strings.Contains(pane, "Enter to view") && strings.Contains(pane, "↑/↓ to select") {
			if err := p.sendClaudeControlKey(name, "Enter"); err != nil {
				return err
			}
		}
		if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("native task control did not open: %w", err)
		}
	}
	for {
		s, err := read()
		if err != nil {
			return err
		}
		if s.TurnID != task.TurnID {
			return fmt.Errorf("native turn changed while waiting for task exit")
		}
		if current, exists := s.Tasks[task.ID]; exists && current.Done {
			break
		}
		if err := sleepWithContext(ctx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("native task exit unconfirmed: %w", err)
		}
	}
	if err := p.tm.noClientAttached(name); err != nil {
		return err
	}
	// Close only the local task control, never submit an extra inference.
	if err := p.sendClaudeControlKey(name, "Escape"); err != nil {
		return err
	}
	open = false
	return p.tm.WaitForIdle(ctx, name, 3*time.Second)
}

func claudeCommandPrefix(command string) string {
	runes := []rune(command)
	if len(runes) > 260 {
		runes = runes[:260]
	}
	return strings.Join(strings.Fields(string(runes)), "")
}

// Every key rechecks attachment; a human taking over ends automation immediately.
func (p *Provider) sendClaudeControlKey(name, key string) error {
	if err := p.tm.noClientAttached(name); err != nil {
		return err
	}
	return p.tm.SendKeysRaw(name, key)
}

// Restrict command matching to the last native details panel, excluding chat
// scrollback and model text that happens to contain a similar command.
func claudeTaskDetailCommand(pane string) string {
	lines := strings.Split(pane, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "Shell details" {
			start = i
		}
	}
	if start < 0 {
		return ""
	}
	var command strings.Builder
	collecting := false
	for _, line := range lines[start+1:] {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Command:") {
			collecting = true
			line = strings.TrimPrefix(line, "Command:")
		}
		if collecting {
			if line == "Output:" {
				break
			}
			command.WriteString(strings.Join(strings.Fields(line), ""))
		}
	}
	return command.String()
}
