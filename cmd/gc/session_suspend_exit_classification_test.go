package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestSuspendIsNoCrashOrChurnOfTheConversation: a suspend stops the runtime on
// purpose, so the legacy tick that next observes it dead must not classify the
// death as a rapid crash (inside stabilityThreshold) or as churn (before
// churnProductivityThreshold). Either classification resets the provider
// conversation (session_key) and counts the suspend against the session.
func TestSuspendIsNoCrashOrChurnOfTheConversation(t *testing.T) {
	const key = "keep-provider-conversation"
	suspends := []struct {
		name  string
		actor session.ActorKind
		soft  bool
	}{
		{"operator", session.ActorOperator, false},
		{"idle auto-suspend", session.ActorController, true},
		{"city stop sweep", session.ActorSweep, false},
	}
	for _, s := range suspends {
		for _, elapsed := range []time.Duration{2 * time.Second, 2 * time.Minute} {
			t.Run(s.name+"/"+elapsed.String(), func(t *testing.T) {
				e := newKilledSeatEnv(t, persistentWorker())
				work := e.addWork(t, "in_progress", e.seat.ID, nil)
				e.tick(t, []beads.Bead{work}, withReadyAssignedFlags([]bool{true}))
				seat := e.reload(t, e.seat.ID)
				name := seat.Metadata["session_name"]
				if !e.sp.IsRunning(name) || seat.Metadata["last_woke_at"] == "" {
					t.Fatalf("seat not woken (running=%v last_woke_at=%q), test premise", e.sp.IsRunning(name), seat.Metadata["last_woke_at"])
				}
				if err := e.mem.SetMetadata(e.seat.ID, "session_key", key); err != nil {
					t.Fatal(err)
				}
				e.advance(elapsed)
				mgr := session.NewManagerWithOptions(e.mem, e.sp, session.WithClock(e.clk), session.WithCityPath(e.city))
				if err := mgr.Suspend(context.Background(), testActorIn(t, s.actor, e.city), e.seat.ID, s.soft); err != nil {
					t.Fatalf("Suspend: %v", err)
				}
				if e.sp.IsRunning(name) {
					t.Fatal("suspend left the runtime running")
				}
				e.advance(time.Second)
				e.tick(t, []beads.Bead{e.reload(t, work.ID)})
				m := e.reload(t, e.seat.ID).Metadata
				if m["session_key"] != key {
					t.Errorf("session_key = %q after the tick, want the suspended conversation %q kept", m["session_key"], key)
				}
				if m["continuation_reset_pending"] != "" {
					t.Errorf("continuation_reset_pending = %q, want no conversation reset", m["continuation_reset_pending"])
				}
				if v := m["wake_attempts"]; v != "" && v != "0" {
					t.Errorf("wake_attempts = %q, want the suspend not counted as a rapid crash", v)
				}
				if v := m["churn_count"]; v != "" && v != "0" {
					t.Errorf("churn_count = %q, want the suspend not counted as churn", v)
				}
			})
		}
	}
}
