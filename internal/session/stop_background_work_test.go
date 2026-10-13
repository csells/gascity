package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Codex keeps running a stopped turn's commands in background terminals. An
// explicit stop has the runtime stop them, once the interrupt boundary is
// reached.
func TestStopTurnStopsCodexBackgroundWorkAfterTheBoundary(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "codex")

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}
	boundary := callIndex(sp.Calls, "WaitForInterruptBoundary", info.SessionName)
	stop := callIndex(sp.Calls, "StopBackgroundWork", info.SessionName)
	if boundary < 0 || stop < 0 || stop < boundary {
		t.Fatalf("calls = %v, want WaitForInterruptBoundary then StopBackgroundWork", callMethods(sp.Calls))
	}
	if got := sp.CountCalls("StopBackgroundWork", info.SessionName); got != 1 {
		t.Fatalf("StopBackgroundWork calls = %d, want 1", got)
	}
}

// A replacement message must not stop background work: Codex's /stop closes
// every background terminal of the session, including an earlier turn's dev
// server.
func TestSubmitInterruptNowLeavesCodexBackgroundWorkRunning(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "codex")

	if _, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "replace the current turn", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow); err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if callIndex(sp.Calls, "WaitForInterruptBoundary", info.SessionName) < 0 {
		t.Fatalf("calls = %v, want the interrupt boundary wait", callMethods(sp.Calls))
	}
	if got := sp.CountCalls("StopBackgroundWork", info.SessionName); got != 0 {
		t.Fatalf("calls = %v, want no StopBackgroundWork for an interrupt_now submit", callMethods(sp.Calls))
	}
}

func TestStopTurnLeavesClaudeBackgroundWorkAlone(t *testing.T) {
	mgr, sp, info := newStopTestSession(t, "claude")

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}
	if got := sp.CountCalls("StopBackgroundWork", info.SessionName); got != 0 {
		t.Fatalf("calls = %v, want no StopBackgroundWork for Claude", callMethods(sp.Calls))
	}
}

// The turn is already stopped when its background work will not stop, so
// StopTurn succeeds and logs it.
func TestStopTurnLogsCodexBackgroundWorkThatWillNotStop(t *testing.T) {
	logs := captureLog(t)
	mgr, sp, info := newStopTestSession(t, "codex")
	sp.BackgroundWorkStopErrors = map[string]error{info.SessionName: errors.New("pane unreadable")}

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn = %v, want nil: the turn was stopped", err)
	}
	if !strings.Contains(logs.String(), "pane unreadable") {
		t.Fatalf("log = %q, want the StopBackgroundWork failure logged", logs.String())
	}
}
