package worker

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type ownedTurnRuntime struct {
	*runtime.Fake
	request runtime.TurnControlRequest
}

func (p *ownedTurnRuntime) ControlTurn(_ context.Context, _ string, request runtime.TurnControlRequest) (runtime.TurnTarget, error) {
	p.request = request
	return runtime.TurnTarget{Mode: "owned_commands", TurnID: request.TurnID, State: "stopped"}, nil
}

func TestSessionHandleControlsOwnedCommandsWithoutWholeSessionStop(t *testing.T) {
	h, store, sp, _ := newTestSessionHandle(t, SessionSpec{Profile: ProfileCodexTmuxCLI, Template: "owned", Command: "codex", Provider: "codex", WorkDir: t.TempDir()})
	control, ok := any(h).(interface {
		ControlTurn(context.Context, runtime.TurnControlRequest) (runtime.TurnTarget, error)
	})
	if !ok {
		t.Fatal("worker handle does not expose owned-turn control")
	}
	p := &ownedTurnRuntime{Fake: sp}
	h.manager = sessionpkg.NewManagerWithOptions(store, p)
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(h.sessionID, "session_key", "exact-native-key"); err != nil {
		t.Fatal(err)
	}
	callCount := len(sp.Calls)
	target, err := control.ControlTurn(context.Background(), runtime.TurnControlRequest{Action: "stop", TurnID: "owned-turn"})
	if err != nil {
		t.Fatal(err)
	}
	if target.State != "stopped" || p.request.SessionKey != "exact-native-key" || p.request.TurnID != "owned-turn" {
		t.Fatalf("wrong owned target: %+v %+v", target, p.request)
	}
	for _, call := range sp.Calls[callCount:] {
		if call.Method == "Interrupt" || call.Method == "Stop" || call.Method == "SendKeys" {
			t.Fatalf("owned Stop fell back to legacy mutation: %+v", call)
		}
	}
}
