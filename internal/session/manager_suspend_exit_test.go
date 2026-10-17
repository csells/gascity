package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// suspendExitObserver runs beforeStop when the suspend stops the runtime, and
// fails that stop with stopErr.
type suspendExitObserver struct {
	*runtime.Fake
	beforeStop func()
	stopErr    error
}

func (p *suspendExitObserver) Stop(name string) error {
	if p.beforeStop != nil {
		p.beforeStop()
	}
	if p.stopErr != nil {
		return p.stopErr
	}
	return p.Fake.Stop(name)
}

// failWakeStampStore fails any write of last_woke_at. It embeds the
// interface, so it offers no conditional writer and every fenced write goes
// through Update.
type failWakeStampStore struct{ beads.Store }

func (s failWakeStampStore) Update(id string, opts beads.UpdateOpts) error {
	if _, ok := opts.Metadata["last_woke_at"]; ok {
		return errors.New("update failed")
	}
	return s.Store.Update(id, opts)
}

func (s failWakeStampStore) SetMetadata(id, key, value string) error {
	if key == "last_woke_at" {
		return errors.New("set metadata failed")
	}
	return s.Store.SetMetadata(id, key, value)
}

func (s failWakeStampStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs["last_woke_at"]; ok {
		return errors.New("set metadata failed")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// suspendKinds are the three Suspend forms that stop a runtime.
var suspendKinds = []struct {
	name    string
	suspend func(*Manager, string) error
}{
	{"operator", operatorSuspend},
	{"idle", func(m *Manager, id string) error {
		return m.Suspend(context.Background(), testActor(m, ActorController), id, true)
	}},
	{"sweep", sweepSuspend},
}

// newWokeSession is a running session with a provider conversation, woken
// woke ago.
func newWokeSession(t *testing.T, woke time.Time) (*Manager, *suspendExitObserver, beads.Store, Info) {
	t.Helper()
	store := beads.NewMemStore()
	sp := &suspendExitObserver{Fake: runtime.NewFake()}
	mgr := newTestManager(t, store, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(info.ID, map[string]string{"session_key": "keep-provider-conversation", "last_woke_at": woke.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	return mgr, sp, store, info
}

// TestSuspendMasksTheStopFromExitClassification: by the time a suspend's stop
// can be observed as a dead runtime, the row reads as no rapid crash and no
// churn, either of which would reset the provider conversation.
func TestSuspendMasksTheStopFromExitClassification(t *testing.T) {
	for _, k := range suspendKinds {
		for _, elapsed := range []time.Duration{time.Second, time.Minute} {
			t.Run(k.name+"/"+elapsed.String(), func(t *testing.T) {
				now := time.Now().UTC()
				mgr, sp, store, info := newWokeSession(t, now.Add(-elapsed))
				check := func(when string) {
					b, err := store.Get(info.ID)
					if err != nil {
						t.Fatal(err)
					}
					outcome := DecideSessionExit(ExitFacts{LastWokeAt: b.Metadata["last_woke_at"], Now: now, StabilityThreshold: 30 * time.Second, ProductivityThreshold: 5 * time.Minute})
					if outcome != ExitNone {
						t.Errorf("%s: suspended runtime's death classified as %v, want ExitNone", when, outcome)
					}
					if b.Metadata["session_key"] != "keep-provider-conversation" {
						t.Errorf("%s: session_key = %q, want the conversation kept", when, b.Metadata["session_key"])
					}
				}
				sp.beforeStop = func() { check("at the stop") }
				if err := k.suspend(mgr, info.ID); err != nil {
					t.Fatalf("Suspend: %v", err)
				}
				check("after the suspend")
			})
		}
	}
}

// TestSuspendRestoresTheWakeStampWhenLiveStopFails: a suspend that cannot
// stop a live runtime leaves its wake stamp, so a later real crash is still
// classified.
func TestSuspendRestoresTheWakeStampWhenLiveStopFails(t *testing.T) {
	for _, k := range suspendKinds {
		t.Run(k.name, func(t *testing.T) {
			woke := time.Now().UTC().Add(-time.Minute)
			mgr, sp, store, info := newWokeSession(t, woke)
			sp.stopErr = errors.New("cannot stop live process")
			if err := k.suspend(mgr, info.ID); !errors.Is(err, sp.stopErr) {
				t.Fatalf("Suspend error = %v, want the stop failure", err)
			}
			b, err := store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := b.Metadata["last_woke_at"]; got != woke.Format(time.RFC3339) {
				t.Errorf("last_woke_at = %q after a failed stop, want %q", got, woke.Format(time.RFC3339))
			}
			if State(b.Metadata["state"]) == StateSuspended {
				t.Error("failed stop left the row suspended over a live runtime")
			}
		})
	}
}

// TestSuspendDoesNotStopWhenTheWakeStampCannotBeCleared: a stop that would
// read as a crash is not made.
func TestSuspendDoesNotStopWhenTheWakeStampCannotBeCleared(t *testing.T) {
	for _, k := range suspendKinds {
		t.Run(k.name, func(t *testing.T) {
			mgr, sp, store, info := newWokeSession(t, time.Now().UTC())
			mgr.store = failWakeStampStore{Store: store}
			stopped := false
			sp.beforeStop = func() { stopped = true }
			if err := k.suspend(mgr, info.ID); err == nil {
				t.Fatal("Suspend succeeded, want the wake-stamp write error")
			}
			if stopped || !sp.IsRunning(info.SessionName) {
				t.Errorf("runtime stopped (stop called=%v running=%v) without masking it from exit classification", stopped, sp.IsRunning(info.SessionName))
			}
		})
	}
}
