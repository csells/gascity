package tmux

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/codexcontrol"
)

// nudgeKeystrokesForFamily composes the keys a nudge actually puts into a pane,
// in NudgeSession's order: the optional pre-submit Escape (step 3) followed by
// the declared submit sequence (step 5). It exists in the test because the
// hazard is the COMPOSITION of two independent tables, which neither table can
// state on its own.
func nudgeKeystrokesForFamily(family string) []string {
	var keys []string
	if !providerEnvSkipsEscape(family) {
		keys = append(keys, "Escape")
	}
	return append(keys, nudgeSubmitKeySequenceForFamily(family)...)
}

// TestCodexNudgeSubmitsWithExactlyOneEscapeThenEnter is the #4706 fix and its
// own guard rail. codex buffers a send-keys burst as a paste, so a lone trailing
// Enter is swallowed as a composer newline and the agent's first turn never
// starts. Escape-then-Enter submits it — but a SECOND Escape would be
// backtrack/edit-previous, so the count matters as much as the keys.
func TestCodexNudgeSubmitsWithExactlyOneEscapeThenEnter(t *testing.T) {
	got := nudgeKeystrokesForFamily("codex")
	want := []string{"Escape", "Enter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codex nudge keystrokes = %v, want %v", got, want)
	}
	if got := nudgeSubmitKeySequenceForFamily("codex"); !reflect.DeepEqual(got, want) {
		t.Fatalf("codex submit sequence = %v, want %v declared as one retryable unit", got, want)
	}
	if !providerEnvSkipsEscape("codex") {
		t.Fatal("codex left providersSkippingEscapeBeforeEnter; combined with its submit entry that sends Escape twice, which codex binds to backtrack rather than submit")
	}
}

// Control: the claude path is byte-identical to what it has always been. If the
// codex entry had been added to a shared default, or the skip list had been
// edited instead of the table, this row would move too.
func TestClaudeNudgeKeystrokesAreUnchanged(t *testing.T) {
	if got, want := nudgeKeystrokesForFamily("claude"), []string{"Enter"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claude nudge keystrokes = %v, want %v", got, want)
	}
	// And an unregistered family keeps the historical default-with-Escape shape.
	if got, want := nudgeKeystrokesForFamily("some-unregistered-family"), []string{"Escape", "Enter"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unregistered family keystrokes = %v, want %v", got, want)
	}
}

// TestCodexSubmitIsVerified pins the second half of the codex fix: its submit is
// CONFIRMED against the busy indicator paneContainsBusyIndicator already reads
// for it ("esc to interrupt"). Without eligibility the best-effort fallback
// reports success the moment keys reach tmux, so the queue acks and deletes an
// item whose paste may still be unsubmitted — losing the nudge outright instead
// of requeueing it under the attempt cap.
func TestCodexSubmitIsVerified(t *testing.T) {
	for _, family := range []string{"claude", "codex"} {
		if !submitVerifyEligibleFamily(family) {
			t.Errorf("submitVerifyEligibleFamily(%q) = false, want true", family)
		}
	}
	// Control: a family whose busy indicator this package cannot read stays on
	// best-effort delivery, because reporting every submit as unconfirmed would
	// burn the queue's attempts re-pasting messages that already landed.
	for _, family := range []string{"grok", "kimi", "opencode", "", "some-unregistered-family"} {
		if submitVerifyEligibleFamily(family) {
			t.Errorf("submitVerifyEligibleFamily(%q) = true, want false", family)
		}
	}
}

// TestCodexBusyIndicatorIsReadable is the premise the eligibility above rests
// on: if codex's indicator string ever stops matching, verification would report
// every codex submit as unconfirmed and the queue would re-paste every nudge
// three times before dead-lettering it.
func TestCodexBusyIndicatorIsReadable(t *testing.T) {
	if !paneContainsBusyIndicator([]string{"  working  (esc to interrupt)"}) {
		t.Fatal("codex's busy indicator no longer matches; submit verification for codex would report every delivery unconfirmed")
	}
	// The footer as codex actually renders it, captured live off codex-cli
	// 0.147.0 by atbrace in #5122. The synthetic string above pins the
	// substring; this pins the real line it has to be found inside.
	if !paneContainsBusyIndicator([]string{"• Working (3s • esc to interrupt)"}) {
		t.Fatal("codex-cli's live Working footer no longer reads as busy; submit verification for codex would report every delivery unconfirmed")
	}
	// The other direction, and the one that matters most: an IDLE codex
	// composer must not read as busy. Verification treats a busy pane as proof
	// the turn started, so a false-busy read on idle chrome means a dropped
	// Enter is never re-sent — the nudge sits unsubmitted in the composer while
	// every liveness surface reads green. Fixture is atbrace's live idle pane
	// from #5122: a finished reply, the empty prompt, and the model/cwd status
	// line whose "·" separator is the near-miss the spinner regex must not take.
	if paneContainsBusyIndicator([]string{
		"• PONG",
		"› Explain this codebase",
		"  gpt-5.5 low · /tmp/probe",
	}) {
		t.Fatal("idle codex composer reads as busy; a lost Enter would never be re-sent and the nudge would stall unsubmitted")
	}
}

// TestNudgeSessionComposesEscapeThenSubmitSequence pins the PROVENANCE of the
// composition nudgeKeystrokesForFamily models.
//
// The two decisions live in NudgeSession's body — the pre-submit Escape at step
// 3, then the declared submit sequence at step 5 — and driving that body needs a
// live tmux pane (both decisions resolve through the pane's environment or a
// process sniff), so a unit test cannot execute it. What it CAN do is refuse to
// let the model drift from the code: if NudgeSession stops consulting either
// decision, or consults them in the other order, the helper above is describing
// something that no longer happens and this fails.
func TestNudgeSessionComposesEscapeThenSubmitSequence(t *testing.T) {
	body := nudgeSessionSource(t)
	escapeAt := strings.Index(body, "t.shouldSendEscapeBeforeEnter(target)")
	submitAt := strings.Index(body, "t.nudgeSubmitKeySequence(target)")
	switch {
	case escapeAt < 0:
		t.Fatal("NudgeSession no longer consults shouldSendEscapeBeforeEnter; nudgeKeystrokesForFamily models a step that is gone")
	case submitAt < 0:
		t.Fatal("NudgeSession no longer consults nudgeSubmitKeySequence; nudgeKeystrokesForFamily models a step that is gone")
	case submitAt < escapeAt:
		t.Fatal("NudgeSession now resolves the submit sequence BEFORE the pre-submit Escape decision; re-derive nudgeKeystrokesForFamily against the new order")
	}
}

// nudgeSessionSource returns the source text of NudgeSession's body.
func nudgeSessionSource(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("tmux.go")
	if err != nil {
		t.Fatalf("reading tmux.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (t *Tmux) NudgeSession(")
	if start < 0 {
		t.Fatal("NudgeSession not found in tmux.go")
	}
	rest := body[start:]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func TestNativeDeliveryConfirmsFastTurnWithoutTerminalBusy(t *testing.T) {
	before := codexcontrol.Target{ThreadID: "thread", TurnID: "previous", State: "completed", Delivery: &codexcontrol.Delivery{TurnID: "previous", ItemID: "old", SHA256: "old"}}
	after := codexcontrol.Target{ThreadID: "thread", TurnID: "quick", State: "completed", Delivery: &codexcontrol.Delivery{TurnID: "quick", ItemID: "new", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("exact prompt")))}}
	for _, tc := range []struct {
		name   string
		mutate func(*codexcontrol.Target)
		want   bool
	}{
		{"fast completed turn", func(*codexcontrol.Target) {}, true},
		{"different thread", func(a *codexcontrol.Target) { a.ThreadID = "other" }, false},
		{"old item", func(a *codexcontrol.Target) { a.Delivery.ItemID = "old" }, false},
		{"different prompt", func(a *codexcontrol.Target) { a.Delivery.SHA256 = "different" }, false},
		{"missing proof", func(a *codexcontrol.Target) { a.Delivery = nil }, false},
		{"lost native stream", func(a *codexcontrol.Target) { a.State = "unconfirmed" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := after
			d := *after.Delivery
			a.Delivery = &d
			tc.mutate(&a)
			if got := nativeDeliveryConfirmed(before, a, "exact prompt"); got != tc.want {
				t.Fatalf("confirmed=%v want %v", got, tc.want)
			}
		})
	}
	var enters int
	confirmed, err := submitEnterUntilNativeOrBusy(func() error { enters++; return nil }, func() {}, func() (bool, error) { return nativeDeliveryConfirmed(before, after, "exact prompt"), nil }, func() (bool, error) { return false, nil }, noSleep)
	if err != nil || !confirmed || enters != 1 {
		t.Fatalf("fast native turn repeated or unconfirmed: confirmed=%v enters=%d err=%v", confirmed, enters, err)
	}
	t.Run("busy prevents retries while native acceptance lags", func(t *testing.T) {
		sends := 0
		confirmed, err := submitEnterUntilNativeOrBusy(
			func() error { sends++; return nil }, func() {},
			func() (bool, error) { return false, nil },
			func() (bool, error) { return true, nil }, noSleep,
		)
		if err != nil || confirmed || sends != 1 {
			t.Fatalf("busy must stop retries without proving acceptance: confirmed=%v sends=%d err=%v", confirmed, sends, err)
		}
	})
	t.Run("lost native control stops keys", func(t *testing.T) {
		sends := 0
		lost := errors.New("native control lost")
		confirmed, err := submitEnterUntilNativeOrBusy(
			func() error { sends++; return nil }, func() {},
			func() (bool, error) { return false, lost },
			func() (bool, error) { return false, nil }, noSleep,
		)
		if !errors.Is(err, lost) || confirmed || sends != 1 {
			t.Fatalf("lost control must stop retries: confirmed=%v sends=%d err=%v", confirmed, sends, err)
		}
	})
	t.Run("delayed native receipt survives terminal recovery budget", func(t *testing.T) {
		start := time.Unix(0, 0)
		clock := start
		calls := 0
		confirmed, err := waitForNativeDelivery(func() (bool, error) {
			calls++
			return clock.Sub(start) >= 11*time.Second, nil
		}, func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) })
		if err != nil || !confirmed || clock.Sub(start) < 11*time.Second || calls < 2 {
			t.Fatalf("delayed receipt rejected: confirmed=%v elapsed=%v err=%v", confirmed, clock.Sub(start), err)
		}
	})
	t.Run("native acceptance wait is bounded and propagates loss", func(t *testing.T) {
		start := time.Unix(0, 0)
		clock := start
		confirmed, err := waitForNativeDelivery(func() (bool, error) { return false, nil }, func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) })
		if err != nil || confirmed || clock.Sub(start) < nativeDeliveryTimeout || clock.Sub(start) > nativeDeliveryTimeout+submitConfirmPollInterval {
			t.Fatalf("unconfirmed receipt wait unbounded: confirmed=%v elapsed=%v err=%v", confirmed, clock.Sub(start), err)
		}
		lost := errors.New("native receipt connection lost")
		confirmed, err = waitForNativeDelivery(func() (bool, error) { return false, lost }, func() time.Time { return clock }, func(time.Duration) { t.Fatal("waited after losing native control") })
		if confirmed || !errors.Is(err, lost) {
			t.Fatalf("lost control hidden: confirmed=%v err=%v", confirmed, err)
		}
	})
}
