package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// manualCreateBead is a session bead as Manager.CreateSession writes it for a
// client's create (POST /sessions): state=active and session_origin=manual from
// the start, no pending-create claim, and no agent_name or creation_complete_at
// yet. creation_complete_at is stamped only once the runtime start succeeds.
func manualCreateBead(env *reconcilerTestEnv, age time.Duration, extra map[string]string) beads.Bead {
	session := env.createSessionBead("s-gc-new", "claude")
	meta := map[string]string{
		"state":          "active",
		"session_origin": "manual",
		"agent_name":     "",
	}
	for k, v := range extra {
		meta[k] = v
	}
	env.setSessionMetadata(&session, meta)
	session.CreatedAt = env.clk.Now().Add(-age)
	return session
}

// TestReconcileSessionBeads_ManualCreateInFlightIsNotOrphaned pins the live
// failure: a provider session created through the API was closed as
// "session orphaned: configured agent removed" 2 s after its create, while
// the runtime was still starting. The tick's desired-state snapshot predated
// the bead and the runtime was not up yet, so the bead looked like a dead
// orphan. Its create had then reported the closed session as active.
func TestReconcileSessionBeads_ManualCreateInFlightIsNotOrphaned(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "claude", Provider: "claude"}}}
	session := manualCreateBead(env, 2*time.Second, nil)

	env.reconcile([]beads.Bead{session})

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("session whose create is still starting it was closed: reason %q", got.Metadata["close_reason"])
	}
}

// The protection lasts only while a start could still be running: a manual
// bead that never confirmed its start and whose runtime is long gone is still
// reaped, as is one whose start completed and whose runtime then died.
func TestReconcileSessionBeads_ManualCreateOrphanStillClosed(t *testing.T) {
	cases := []struct {
		name  string
		age   time.Duration
		extra map[string]string
	}{
		{name: "never confirmed, past the start window", age: 30 * time.Minute},
		{name: "start confirmed, runtime gone", age: 2 * time.Second, extra: map[string]string{"creation_complete_at": "2026-03-08T11:59:58Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "claude", Provider: "claude"}}}
			session := manualCreateBead(env, tc.age, tc.extra)

			env.reconcile([]beads.Bead{session})

			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("Get session: %v", err)
			}
			if got.Status != "closed" {
				t.Fatalf("orphaned manual session left open: state %q", got.Metadata["state"])
			}
		})
	}
}
