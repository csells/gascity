// Package codexcontrol owns native Codex command cancellation behind GC's runtime boundary.
package codexcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Peer calls the native app-server that owns the commands.
type Peer interface {
	Call(context.Context, string, any, any) error
}

// Target is a fenced cancellation target. TurnID must be echoed by the caller.
type Target struct {
	ThreadID string `json:"thread_id"`
	TurnID   string `json:"turn_id"`
	State    string `json:"state"`
}
type command struct {
	item, turn       string
	detached, exited bool
}
type thread struct {
	current, status     string
	pending             int
	stopping, uncertain bool
	commands            map[string]command
	stopped             map[string]bool
}

// Controller retains ownership from the native event stream for its entire lifetime.
type Controller struct {
	mu      sync.Mutex
	peer    Peer
	threads map[string]*thread
	changed chan struct{}
	failure error
}

// New creates a controller before the native client starts any turns.
func New(peer Peer) *Controller {
	return &Controller{peer: peer, threads: map[string]*thread{}, changed: make(chan struct{})}
}

func (c *Controller) thread(id string) *thread {
	t := c.threads[id]
	if t == nil {
		t = &thread{commands: map[string]command{}, stopped: map[string]bool{}}
		c.threads[id] = t
	}
	return t
}
func (c *Controller) notify() { close(c.changed); c.changed = make(chan struct{}) }

// Fail fences all further control after the authoritative stream is lost.
func (c *Controller) Fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure == nil {
		c.failure = err
	}
	c.notify()
}

// Observe records ownership before forwarding a native notification to the TUI.
func (c *Controller) Observe(method string, raw json.RawMessage) error {
	if method != "turn/started" && method != "turn/completed" && method != "item/started" && method != "item/completed" {
		return nil
	}
	var e struct {
		ThreadID string                      `json:"threadId"`
		TurnID   string                      `json:"turnId"`
		Turn     struct{ ID, Status string } `json:"turn"`
		Item     struct {
			Type, ID, ProcessID, Status string
			ExitCode                    *int `json:"exitCode"`
		} `json:"item"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		c.Fail(err)
		return err
	}
	if e.ThreadID == "" {
		err := errors.New("native event has no thread identity")
		c.Fail(err)
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	defer c.notify()
	t := c.thread(e.ThreadID)
	switch method {
	case "turn/started":
		if e.Turn.ID == "" {
			return errors.New("native turn has no identity")
		}
		if t.stopping && t.current != e.Turn.ID {
			t.uncertain = true
		}
		t.current = e.Turn.ID
		t.status = e.Turn.Status
	case "turn/completed":
		if t.current == e.Turn.ID {
			t.status = e.Turn.Status
		}
	default:
		if e.Item.Type != "commandExecution" || e.Item.ProcessID == "" {
			return nil
		}
		if e.TurnID == "" || e.Item.ID == "" {
			t.uncertain = true
			return errors.New("native command ownership missing")
		}
		old, ok := t.commands[e.Item.ProcessID]
		if ok && old.item != e.Item.ID && !old.exited {
			t.uncertain = true
			return errors.New("native live process identity reused")
		}
		detached := ok && old.item == e.Item.ID && old.detached
		t.commands[e.Item.ProcessID] = command{item: e.Item.ID, turn: e.TurnID, detached: detached, exited: method == "item/completed" && e.Item.ExitCode != nil}
	}
	return nil
}

// BeginStart fences native submissions while Stop owns or has uncertain ownership of a turn.
func (c *Controller) BeginStart(id string) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.thread(id)
	if c.failure != nil {
		return nil, c.failure
	}
	if t.stopping || t.uncertain {
		return nil, errors.New("command stop is in progress or unconfirmed")
	}
	t.pending++
	return func() { c.mu.Lock(); defer c.mu.Unlock(); t.pending--; c.notify() }, nil
}

// Target snapshots the current turn; it never guesses a conversation from filesystem recency.
func (c *Controller) Target(id string) Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.thread(id)
	state := t.status
	switch {
	case c.failure != nil || t.uncertain:
		state = "unconfirmed"
	case t.stopping:
		state = "stopping"
	case t.pending > 0:
		state = "submitting"
	case t.stopped[t.current]:
		state = "stopped"
	case t.current == "":
		state = "idle"
	}
	return Target{ThreadID: id, TurnID: t.current, State: state}
}

// Detach explicitly transfers a command out of automatic turn cancellation.
func (c *Controller) Detach(id, turn, pid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.thread(id)
	if c.failure != nil {
		return c.failure
	}
	if t.stopping || t.uncertain {
		return errors.New("cannot detach during unconfirmed Stop")
	}
	cmd, ok := t.commands[pid]
	if !ok || cmd.turn != turn || cmd.exited {
		return errors.New("detach target is not a known live command in this turn")
	}
	cmd.detached = true
	t.commands[pid] = cmd
	return nil
}

// Stop interrupts exactly one turn and confirms termination of each owned command.
// An uncertain outcome keeps future submissions fenced instead of claiming success.
func (c *Controller) Stop(ctx context.Context, id, turn string) (err error) {
	c.mu.Lock()
	t := c.thread(id)
	if t.stopped[turn] {
		c.mu.Unlock()
		return nil
	}
	if c.failure != nil {
		err = c.failure
		c.mu.Unlock()
		return err
	}
	if turn == "" || t.current != turn {
		c.mu.Unlock()
		return errors.New("native turn target changed or is unknown")
	}
	if t.pending > 0 || t.stopping || t.uncertain {
		c.mu.Unlock()
		return errors.New("native submission or cancellation is unconfirmed")
	}
	t.stopping = true
	preserve := []string{}
	for pid, cmd := range t.commands {
		if cmd.turn == turn && cmd.detached && !cmd.exited {
			preserve = append(preserve, pid)
		}
	}
	sort.Strings(preserve)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		t.stopping = false
		if err != nil {
			t.uncertain = true
		} else {
			t.stopped[turn] = true
		}
		c.notify()
	}()
	// This native operation fences starts before interrupting the turn, retains
	// handles before command-start notification, and waits for confirmed exit.
	// A turn-interrupted event followed by an empty list is NOT a barrier.
	var reply StopTurnResult
	if err = c.peer.Call(ctx, "thread/backgroundTerminals/stopTurn", StopTurnRequest{id, turn, preserve}, &reply); err != nil {
		return fmt.Errorf("native command ownership barrier unavailable or failed: %w", err)
	}
	if reply.TurnID != turn {
		return errors.New("native command stop returned a different turn identity")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return c.failure
	}
	if t.uncertain || t.current != turn {
		return errors.New("native turn changed during Stop")
	}
	for _, pid := range reply.Terminated {
		if cmd, ok := t.commands[pid]; ok {
			cmd.exited = true
			t.commands[pid] = cmd
		}
	}
	return nil
}

// StopTurnRequest requires a native start fence, interruption, and confirmed exits.
type StopTurnRequest struct {
	ThreadID          string   `json:"threadId"`
	TurnID            string   `json:"turnId"`
	PreserveProcesses []string `json:"preserveProcesses"`
}

// StopTurnResult acknowledges the exact fenced turn, never an inferred idle state.
type StopTurnResult struct {
	TurnID     string   `json:"turnId"`
	Terminated []string `json:"terminated"`
}

// InitialTarget proves there is no previous turn before the startup hook has
// published a native conversation key. It never selects a thread implicitly.
func (c *Controller) InitialTarget() (Target, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return Target{}, c.failure
	}
	for _, t := range c.threads {
		if t.current != "" || t.pending > 0 || t.uncertain || t.stopping {
			return Target{}, errors.New("native turn exists without persisted conversation identity")
		}
	}
	return Target{State: "initializing"}, nil
}
