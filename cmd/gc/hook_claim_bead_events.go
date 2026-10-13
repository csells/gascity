package main

// bead.* announcement for the writes `gc hook --claim` makes to a work store.
//
// The hook claims a step from a one-shot worker process by writing the agent's
// work store directly (a bd subprocess, hookClaimBdStoreContext). Nothing in
// that path records a bead.* event, and the supervisor's run views
// (GET /runs, /runs/{id}, /runs/{id}/steps) are a fold of exactly those events
// in <city>/.gc/events.jsonl. So a claimed step stayed pending with no assignee
// in the run views until the supervisor happened to read the row back: a live
// read that refetched it, or its cache reconcile 30 to 120 seconds later.
//
// Every successful work-store write the hook makes therefore appends the
// bead.updated the supervisor's CachingStore would have recorded for it, built
// by the one-shot emitter the relocated class stores already use
// (emittingClassStore): the canonical snapshot from beads.EncodeBeadEventPayload,
// the run/session/step correlation on the envelope, and an explicit edge set so
// the supervisor's cache, which applies the event too, does not read it as
// edge coverage unknown. The run tailer folds it on its next poll, about a
// second later.
//
// The writes covered are the claim CAS (fresh claims and ready-assignment
// promotions), the execution-identity stamp, continuation preassignment, the
// adoption re-stamp, the undelivered-claim release and the stale-lease reclaim.
// A write through the relocated class binding (claim_class_route.go) is not
// announced here: that store is an emittingClassStore and announces itself.
//
// Recording is best-effort. The write has already committed, so a log that
// cannot be opened, a snapshot that cannot be read or an append that times out
// on the log's lock leaves the write in place and is reported once on stderr
// (warnClassStoreEmit); the supervisor's read-back or reconcile still catches
// the change up as before. The append itself goes through the city's
// secondary FileRecorder (openCityEventsLog), which serializes it with the
// supervisor's own appends under the log's cross-process lock and allocates
// its seq there, so the supervisor does not need to be running.
//
// A repeat is harmless. The supervisor may announce the same change when it
// reads the row back; the run fold is last-snapshot-wins per bead, so a second
// identical snapshot leaves the projection where the first put it.

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// hookClaimAnnounceTimeout bounds the reads an announcement makes (the
// post-write snapshot and its edge confirmation). An announcement that runs
// out is dropped, never retried: the write it describes has already landed.
var hookClaimAnnounceTimeout = 3 * time.Second

// withHookClaimBeadEvents returns ops whose work-store writes announce
// themselves to cityPath's event log. cityPath is the city whose supervisor
// projects these stores: every leg the hook federates (the agent's own store,
// its rigs, the city store) belongs to it, and the supervisor records every
// one of their caches into this one log.
//
// It wraps the seams applyDefaults resolves, so it must run before
// classRoutedHookClaimOps: the class route escalates to the binding only
// after a work-store write fails, and a failed write announces nothing.
func withHookClaimBeadEvents(ops hookClaimOps, cityPath string) hookClaimOps {
	cityPath = strings.TrimSpace(cityPath)
	if cityPath == "" {
		return ops
	}
	ops.applyDefaults()
	base := ops
	announce := func(dir string, env []string, actor, beadID string) {
		announceHookClaimWorkStoreChange(cityPath, dir, env, actor, beadID, nil)
	}

	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		claimed, ok, err := base.Claim(ctx, dir, env, beadID, assignee)
		if ok {
			// A committed claim whose canonical readback failed (ok with err)
			// is still a committed claim; the announcement re-reads it.
			var known *beads.Bead
			if err == nil && strings.TrimSpace(claimed.ID) == beadID {
				known = &claimed
			}
			announceHookClaimWorkStoreChange(cityPath, dir, env, assignee, beadID, known)
		}
		return claimed, ok, err
	}
	ops.StampWorkMeta = func(ctx context.Context, dir string, env []string, beadID, assignee string, patch map[string]string) error {
		err := base.StampWorkMeta(ctx, dir, env, beadID, assignee, patch)
		if err == nil {
			announce(dir, env, assignee, beadID)
		}
		return err
	}
	ops.AssignContinuation = func(ctx context.Context, dir string, env []string, beadID, assignee string) error {
		err := base.AssignContinuation(ctx, dir, env, beadID, assignee)
		if err == nil {
			announce(dir, env, assignee, beadID)
		}
		return err
	}
	ops.RestampAdopted = func(ctx context.Context, dir string, env []string, beadID, fromAssignee, toAssignee string) (bool, error) {
		moved, err := base.RestampAdopted(ctx, dir, env, beadID, fromAssignee, toAssignee)
		if moved && err == nil {
			announce(dir, env, toAssignee, beadID)
		}
		return moved, err
	}
	ops.Release = func(ctx context.Context, dir string, env []string, beadID, assignee string) (bool, error) {
		released, err := base.Release(ctx, dir, env, beadID, assignee)
		if released && err == nil {
			announce(dir, env, assignee, beadID)
		}
		return released, err
	}
	ops.ReclaimStale = func(ctx context.Context, dir string, env []string, beadID string) (bool, string, error) {
		reclaimed, previousOwner, err := base.ReclaimStale(ctx, dir, env, beadID)
		if reclaimed && err == nil {
			announce(dir, env, "", beadID)
		}
		return reclaimed, previousOwner, err
	}
	return ops
}

// announceHookClaimWorkStoreChange records the bead.updated for one landed
// work-store write, reading the row through the same bd context the write
// used. known, when set, is the post-write row the caller already read, which
// spares the re-read.
func announceHookClaimWorkStoreChange(cityPath, dir string, env []string, actor, beadID string, known *beads.Bead) {
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimAnnounceTimeout)
	defer cancel()
	store := hookClaimBdStoreContext(ctx, dir, env, actor)
	if known != nil {
		announceHookClaimBeadChange(cityPath, store, *known)
		return
	}
	(&emittingClassStore{Store: store, cityPath: cityPath}).emitUpdated(beadID)
}

// announceHookClaimBeadChange records bead as a bead.updated in cityPath's
// event log, confirming its edge set through store first. It is the
// supervisor's own bead.updated for that row: same type, payload encoding and
// envelope correlation (emittingClassStore.emit).
func announceHookClaimBeadChange(cityPath string, store beads.Store, bead beads.Bead) {
	emitter := &emittingClassStore{Store: store, cityPath: cityPath}
	emitter.emit(classStoreEmission{eventType: events.BeadUpdated, bead: emitter.withConfirmedEdges(bead)})
}
