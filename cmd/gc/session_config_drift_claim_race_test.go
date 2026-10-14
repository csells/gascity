package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// configDriftDrainInFlight sets up a pool session whose config drifted and
// whose config-drift drain the reconciler already began on an earlier tick,
// before the session's claim landed. ackSet reports whether that drain also
// already published its reconciler drain ack (the tick after the begin).
func configDriftDrainInFlight(t *testing.T, ackSet bool) (*reconcilerTestEnv, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "new-cmd",
			MaxActiveSessions: intPtr(1),
		}},
	}
	env.desiredState["worker"] = TemplateParams{
		TemplateName: "worker",
		InstanceName: "worker",
		Alias:        "worker",
		Command:      "new-cmd",
	}
	oldRuntime := runtime.Config{Command: "old-cmd"}
	if err := env.sp.Start(context.Background(), "worker", oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(oldRuntime),
		"started_live_hash":   runtime.LiveFingerprint(oldRuntime),
	})

	if !beginSessionDrainInfo(env.sessionInfo(session.ID), env.sp, env.dt, "config-drift", env.clk, defaultDrainTimeout) {
		t.Fatal("beginSessionDrainInfo did not begin the config-drift drain")
	}
	if ackSet {
		ds := env.dt.get(session.ID)
		if err := setReconcilerDrainAckMetadata(env.sp, "worker", ds); err != nil {
			t.Fatalf("setReconcilerDrainAckMetadata: %v", err)
		}
		ds.ackSet = true
	}

	// The worker's `gc hook --claim` lands after the drain began: the session
	// row still read awake, so the claim fence (state == draining) let it in.
	if _, err := env.store.Create(beads.Bead{
		Title:    "claimed step",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create(claimed work): %v", err)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	return env, got
}

func assertConfigDriftDrainAbandoned(t *testing.T, env *reconcilerTestEnv, session beads.Bead) {
	t.Helper()
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("config-drift drain still in flight after a claim landed on the session: %+v\nstdout=%s\nstderr=%s", ds, env.stdout.String(), env.stderr.String())
	}
	if ack, _ := env.sp.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Errorf("GC_DRAIN_ACK = %q after a claim landed, want cleared", ack)
	}
	if !env.sp.IsRunning("worker") {
		t.Error("session holding a fresh claim was stopped for config drift")
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if state := got.Metadata["state"]; state == "draining" {
		t.Errorf("session state = %q (reason %q), want it to keep running its claimed work", state, got.Metadata["state_reason"])
	}
}

// TestConfigDriftDrainAck_AbandonedWhenClaimLandsBeforeStop covers the commit
// point: the drain began and published its ack while the session was idle,
// then the session claimed work. The drain-ack arm that turns the ack into a
// stop must re-check the same live-assigned-work guard that gated the begin,
// and abandon the drain instead of stopping a session mid-claim.
func TestConfigDriftDrainAck_AbandonedWhenClaimLandsBeforeStop(t *testing.T) {
	env, session := configDriftDrainInFlight(t, true)
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, newDrainOps(env.sp))
	assertConfigDriftDrainAbandoned(t, env, session)
}

// TestConfigDriftDrain_AbandonedWhenLiveWorkFoundBeforeAck covers the tick
// between the begin and the ack: the drift check finds live assigned work and
// skips beginning a drain, and must also abandon the one already in flight
// rather than let the drain scan publish its ack.
func TestConfigDriftDrain_AbandonedWhenLiveWorkFoundBeforeAck(t *testing.T) {
	env, session := configDriftDrainInFlight(t, false)
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, newDrainOps(env.sp))
	assertConfigDriftDrainAbandoned(t, env, session)
}

// TestConfigDriftDrainAck_StillStopsIdleSession guards that the abandon is
// keyed on live work: an idle drifted session still drains for config drift.
func TestConfigDriftDrainAck_StillStopsIdleSession(t *testing.T) {
	env, session := configDriftDrainInFlight(t, true)
	work, err := env.store.List(beads.ListQuery{Assignee: session.ID})
	if err != nil {
		t.Fatalf("List(work): %v", err)
	}
	for _, b := range work {
		if err := env.store.Close(b.ID); err != nil {
			t.Fatalf("Close(%s): %v", b.ID, err)
		}
	}
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, newDrainOps(env.sp))
	env.stops.wait(-1)
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Metadata["state"] != "draining" && env.sp.IsRunning("worker") {
		t.Fatalf("idle drifted session neither stop-pending nor stopped: state=%q reason=%q\nstdout=%s\nstderr=%s",
			got.Metadata["state"], got.Metadata["state_reason"], env.stdout.String(), env.stderr.String())
	}
}

// claimAtDrainMarkStore lands the worker's claim at the moment the drain-ack
// arm writes its stop-pending mark: after the arm's pre-mark work check read an
// idle seat, and before the draining row the claim fence reads is visible. It
// is the interleaving a pre-check alone cannot see. afterMark, when set, runs
// right after the mark landed, as another writer would.
type claimAtDrainMarkStore struct {
	beads.Store
	sessionID string
	claim     func()
	afterMark func()
	fired     bool
}

func (s *claimAtDrainMarkStore) SetMetadataBatch(id string, kvs map[string]string) error {
	mark := !s.fired && id == s.sessionID && kvs["state"] == "draining"
	if mark {
		s.fired = true
		s.claim()
	}
	if err := s.Store.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	if mark && s.afterMark != nil {
		s.afterMark()
	}
	return nil
}

// configDriftDrainAckWithClaimAtMark is configDriftDrainInFlight with an acked
// drain on an idle seat whose claim lands during the stop-pending mark.
func configDriftDrainAckWithClaimAtMark(t *testing.T, afterMark func(env *reconcilerTestEnv, id string)) (*reconcilerTestEnv, beads.Bead) {
	t.Helper()
	env, session := configDriftDrainInFlight(t, true)
	work, err := env.store.List(beads.ListQuery{Assignee: session.ID})
	if err != nil {
		t.Fatalf("List(work): %v", err)
	}
	for _, b := range work {
		if err := env.store.Close(b.ID); err != nil {
			t.Fatalf("Close(%s): %v", b.ID, err)
		}
	}
	inner := env.store
	wrapped := &claimAtDrainMarkStore{Store: inner, sessionID: session.ID}
	wrapped.claim = func() {
		if _, err := inner.Create(beads.Bead{Title: "claimed at mark", Type: "task", Status: "in_progress", Assignee: session.ID}); err != nil {
			t.Errorf("Create(claimed work): %v", err)
		}
	}
	if afterMark != nil {
		wrapped.afterMark = func() { afterMark(env, session.ID) }
	}
	env.store = wrapped
	return env, session
}

// TestConfigDriftDrainAck_AbandonedWhenClaimLandsDuringStopPendingMark closes
// the last window: the claim's fence read the row before the mark and its CAS
// landed after the arm's pre-check. The arm's post-mark re-read must find the
// claim, revert the mark and queue no stop.
func TestConfigDriftDrainAck_AbandonedWhenClaimLandsDuringStopPendingMark(t *testing.T) {
	env, session := configDriftDrainAckWithClaimAtMark(t, nil)
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, newDrainOps(env.sp))
	env.stops.wait(-1)
	assertConfigDriftDrainAbandoned(t, env, session)
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Metadata["state_reason"] == "drain-ack-stop-pending" || got.Metadata["drain_at"] != "" {
		t.Errorf("stop-pending mark not reverted: state=%q state_reason=%q drain_at=%q", got.Metadata["state"], got.Metadata["state_reason"], got.Metadata["drain_at"])
	}
	work, err := env.store.List(beads.ListQuery{Assignee: session.ID})
	if err != nil {
		t.Fatalf("List(work): %v", err)
	}
	if len(work) != 1 || work[0].Status == "closed" {
		t.Errorf("claimed work assigned to the session = %+v, want the one claim kept", work)
	}
}

// TestConfigDriftDrainAck_PostMarkRevertNeverOverwritesAnotherWriter pins the
// revert's fence: it lands only on the exact stop-pending row the arm wrote. A
// writer that moved the row after the mark (here a different drain's reason)
// keeps its state.
func TestConfigDriftDrainAck_PostMarkRevertNeverOverwritesAnotherWriter(t *testing.T) {
	const otherReason = "other-drain"
	env, session := configDriftDrainAckWithClaimAtMark(t, func(env *reconcilerTestEnv, id string) {
		inner := env.store.(*claimAtDrainMarkStore).Store
		if err := inner.SetMetadata(id, "state_reason", otherReason); err != nil {
			t.Errorf("SetMetadata(state_reason): %v", err)
		}
	})
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, newDrainOps(env.sp))
	env.stops.wait(-1)
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Metadata["state"] != "draining" || got.Metadata["state_reason"] != otherReason {
		t.Fatalf("revert overwrote another writer: state=%q state_reason=%q, want draining/%s", got.Metadata["state"], got.Metadata["state_reason"], otherReason)
	}
}
