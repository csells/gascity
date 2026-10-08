package codexcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type Process struct{ ItemID, ProcessID string }

type peer struct {
	controller *Controller
	processes  []Process
	terminated []string
	fail       bool
}

func (p *peer) Call(_ context.Context, method string, params, result any) error {
	switch method {
	case "thread/backgroundTerminals/stopTurn":
		v := params.(StopTurnRequest)
		if p.fail {
			return errors.New("native barrier unavailable")
		}
		for _, x := range p.processes {
			if x.ProcessID == "foreign" {
				return errors.New("native ownership unknown")
			}
		}
		for _, x := range p.processes {
			keep := false
			for _, id := range v.PreserveProcesses {
				if id == x.ProcessID {
					keep = true
				}
			}
			if !keep {
				p.terminated = append(p.terminated, x.ProcessID)
			}
		}
		if err := p.controller.Observe("turn/completed", json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","status":"interrupted"}}`)); err != nil {
			return err
		}
		b, err := json.Marshal(StopTurnResult{TurnID: v.TurnID, Terminated: p.terminated})
		if err != nil {
			return err
		}
		return json.Unmarshal(b, result)
	default:
		return errors.New("unexpected native method " + method)
	}
}

func fixture(t *testing.T) (*Controller, *peer) {
	t.Helper()
	p := &peer{}
	c := New(p)
	p.controller = c
	if err := c.Observe("turn/started", json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","status":"inProgress"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Observe("item/started", json.RawMessage(`{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"work-item","processId":"work","status":"inProgress"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Observe("item/started", json.RawMessage(`{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"service-item","processId":"service","status":"inProgress"}}`)); err != nil {
		t.Fatal(err)
	}
	p.processes = []Process{{ItemID: "work-item", ProcessID: "work"}, {ItemID: "service-item", ProcessID: "service"}}
	return c, p
}

func TestStopTerminatesOwnedWorkAndPreservesExplicitDetach(t *testing.T) {
	c, p := fixture(t)
	ctx := context.Background()
	if err := c.Detach("thread", "turn", "service"); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(ctx, "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	if len(p.terminated) != 1 || p.terminated[0] != "work" {
		t.Fatalf("terminated %v", p.terminated)
	}
	if err := c.Stop(ctx, "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	if len(p.terminated) != 1 {
		t.Fatal("repeat Stop acted again")
	}
	if err := c.Observe("turn/started", json.RawMessage(`{"threadId":"thread","turn":{"id":"new","status":"inProgress"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(ctx, "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	if c.Target("thread").TurnID != "new" {
		t.Fatal("old Stop affected new turn")
	}
}

func TestStopRejectsUnknownProcessOwnership(t *testing.T) {
	c, p := fixture(t)
	p.processes = append(p.processes, Process{ItemID: "unknown", ProcessID: "foreign"})
	if err := c.Stop(context.Background(), "thread", "turn"); err == nil {
		t.Fatal("unknown ownership reported success")
	}
	if len(p.terminated) != 0 {
		t.Fatal("killed work before ownership validation")
	}
}

func TestStopDoesNotClaimSuccessAfterNativeFailure(t *testing.T) {
	c, p := fixture(t)
	p.fail = true
	if err := c.Stop(context.Background(), "thread", "turn"); err == nil {
		t.Fatal("lost termination reply reported success")
	}
	if c.Target("thread").State != "unconfirmed" {
		t.Fatal("lost stop did not fence next turn")
	}
}

func TestStopRejectsMismatchedNativeTurn(t *testing.T) {
	c, p := fixture(t)
	if err := c.Stop(context.Background(), "thread", "stale"); err == nil {
		t.Fatal("stale target accepted")
	}
	if len(p.terminated) != 0 {
		t.Fatal("stale stop terminated work")
	}
}

func TestSubmissionIsFencedDuringStopAndAfterFailure(t *testing.T) {
	c, _ := fixture(t)
	finish, err := c.BeginStart("thread")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Stop(context.Background(), "thread", "turn"); err == nil {
		t.Fatal("accepted Stop while native submission is in flight")
	}
	finish()
	c.Fail(errors.New("lost event stream"))
	if _, err = c.BeginStart("thread"); err == nil {
		t.Fatal("accepted submission without ownership stream")
	}
}

func TestDetachCannotClaimAnotherTurnOrUnknownProcess(t *testing.T) {
	c, _ := fixture(t)
	if err := c.Detach("thread", "other", "work"); err == nil {
		t.Fatal("cross-turn detach accepted")
	}
	if err := c.Detach("thread", "turn", "unknown"); err == nil {
		t.Fatal("unknown process detached")
	}
}

func TestStopRequiresNativeStartBarrierEvenWithNoObservedCommands(t *testing.T) {
	c, p := fixture(t)
	c.mu.Lock()
	c.threads["thread"].commands = map[string]command{}
	c.mu.Unlock()
	// A command may be running before its notification. The native owner must
	// confirm its exit even though GC has never seen the process handle.
	if err := c.Stop(context.Background(), "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	if len(p.terminated) != 2 {
		t.Fatalf("unobserved commands escaped barrier: %v", p.terminated)
	}
	c, p = fixture(t)
	p.processes = nil
	p.fail = true
	if err := c.Stop(context.Background(), "thread", "turn"); err == nil {
		t.Fatal("empty inventory substituted for unsupported native barrier")
	}
}
