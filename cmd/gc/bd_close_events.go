package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// announceBdPassthroughCloses records bead.closed for each bead a successful
// `gc bd close` (or `gc bd update --status closed`) passthrough just closed.
//
// The passthrough hands the close to a bd subprocess against the work store,
// the way the stock formulas tell a worker to finish its step, and that write
// announces nothing. The supervisor's run views fold only the city event log,
// so the step stayed active there until the supervisor read the row back. This
// is the close half of hook_claim_bead_events.go and uses the same one-shot
// emitter: the canonical post-close snapshot with its edges confirmed, the
// run correlation on the envelope, appended under the log's cross-process lock.
//
// Only a real open-to-closed transition is announced. The supervisor runs a
// committed close's completion and autoclose effects for every bead.closed
// from a writer that is not its cache, so a bead that was already closed
// before the passthrough (per the exact-ID guard's pre-read) gets nothing, and
// neither does one whose read-back does not show it closed. A read-back that
// fails skips the row rather than recording a bare id, which would clobber the
// step's title and run membership in the fold.
//
// A bulk close (the maintenance orders close stale wisps in batches) reads
// its rows back in one batched read and confirms their edges in one more,
// rather than one or two bd forks per id, for the reason the exact-ID guard
// batches its pre-read.
//
// Best-effort: the close has committed. Failures are reported once on stderr
// (warnClassStoreEmit) and the supervisor's read-back still catches the close
// up as before. A close of a bead in a relocated class never reaches here:
// maybeRouteBdByID serves it in process through a store that announces its own
// writes. A `bd close` the agent runs directly, without gc, is not seen by gc
// at all and still waits for the supervisor's read-back.
func announceBdPassthroughCloses(cityPath string, store beads.Store, before map[string]beads.Bead, bdArgs []string) {
	cityPath = strings.TrimSpace(cityPath)
	if cityPath == "" || store == nil {
		return
	}
	ids, ok := workRecordCloseTargets(bdArgs)
	if !ok {
		return
	}
	targets := make([]string, 0, len(ids))
	for _, id := range ids {
		if prior, read := before[id]; read && beadStatusIsClosed(prior.Status) {
			continue
		}
		targets = append(targets, id)
	}
	if len(targets) == 0 {
		return
	}
	emitter := &emittingClassStore{Store: store, cityPath: cityPath}
	closed := closedSnapshots(emitter, targets)
	emissions := make([]classStoreEmission, 0, len(closed))
	for _, bead := range closed {
		emissions = append(emissions, classStoreEmission{eventType: events.BeadClosed, bead: bead})
	}
	emitter.emit(emissions...)
}

// closedSnapshots reads ids back after a close and returns, in input order,
// the ones that now read closed, each with its edge set confirmed.
func closedSnapshots(emitter *emittingClassStore, ids []string) []beads.Bead {
	read, err := beads.PrefetchExact(emitter.Store, ids)
	if err != nil {
		read = map[string]beads.Bead{}
	}
	var closed []beads.Bead
	var edgeless []string
	for _, id := range ids {
		bead, ok := read[id]
		if !ok {
			got, err := emitter.Get(id)
			if err != nil || strings.TrimSpace(got.ID) == "" {
				warnClassStoreEmit(fmt.Errorf("bead.closed skipped: %s could not be re-read after gc bd closed it", id))
				continue
			}
			bead = got
		}
		if !beadStatusIsClosed(bead.Status) {
			continue
		}
		if len(bead.Dependencies) == 0 {
			edgeless = append(edgeless, bead.ID)
		}
		closed = append(closed, bead)
	}
	edges, batched := batchDownEdges(emitter.Store, edgeless)
	for i := range closed {
		bead := closed[i]
		if len(bead.Dependencies) > 0 {
			bead.Needs = nil
		} else if batched {
			bead.Dependencies = edges[bead.ID]
			if bead.Dependencies == nil {
				bead.Dependencies = []beads.Dep{}
			}
			bead.Needs = nil
		} else {
			bead = emitter.withConfirmedEdges(bead)
		}
		closed[i] = bead
	}
	return closed
}

// batchDownEdges reads the DOWN edges of several beads in one round trip, and
// reports false when it could not (one id, no batch capability, or a failed
// read), leaving the caller to confirm each bead's edges on its own. The beads
// were just read back, so an id the batch answered nothing for has no edges.
func batchDownEdges(store beads.Store, ids []string) (map[string][]beads.Dep, bool) {
	lister, ok := store.(beads.DependencyBatchLister)
	if !ok || len(ids) < 2 {
		return nil, false
	}
	edges, err := lister.DepListBatch(ids)
	if err != nil || edges == nil {
		if err != nil && !errors.Is(err, beads.ErrDepListBatchUnsupported) {
			warnClassStoreEmit(fmt.Errorf("confirming the edges of %d closed beads: %w", len(ids), err))
		}
		return nil, false
	}
	return edges, true
}
