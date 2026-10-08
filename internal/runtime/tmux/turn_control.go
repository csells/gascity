package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/codexcontrol"
)

// ControlTurn addresses the GC-owned native adapter for this exact incarnation.
func (p *Provider) ControlTurn(ctx context.Context, name string, req runtime.TurnControlRequest) (runtime.TurnTarget, error) {
	token, err := p.GetMeta(name, "GC_INSTANCE_TOKEN")
	if err != nil {
		return runtime.TurnTarget{}, fmt.Errorf("reading runtime incarnation: %w", err)
	}
	reply, err := codexcontrol.Control(ctx, name, token, codexcontrol.Request{Action: req.Action, ThreadID: req.SessionKey, TurnID: req.TurnID, ProcessID: req.ProcessID})
	if errors.Is(err, os.ErrNotExist) {
		return runtime.TurnTarget{}, fmt.Errorf("%w: this runtime has no native command-control socket", runtime.ErrInteractionUnsupported)
	}
	return runtime.TurnTarget{Mode: "owned_commands", SessionKey: reply.Target.ThreadID, TurnID: reply.Target.TurnID, State: reply.Target.State}, err
}
