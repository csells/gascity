package tmux

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/codexcontrol"
)

// ControlTurn addresses the GC-owned native adapter for this exact incarnation.
func (p *Provider) ControlTurn(ctx context.Context, name string, req runtime.TurnControlRequest) (runtime.TurnTarget, error) {
	if req.ProviderKind == "claude" {
		return p.controlClaudeTurn(ctx, name, req)
	}
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

func nativeDeliveryConfirmed(before, after codexcontrol.Target, message string) bool {
	if before.ThreadID == "" || after.ThreadID != before.ThreadID || after.State == "unconfirmed" || after.State == "initializing" || after.Delivery == nil {
		return false
	}
	d := after.Delivery
	if d.ItemID == "" || d.TurnID == "" || d.TurnID != after.TurnID || (before.Delivery != nil && d.ItemID == before.Delivery.ItemID) {
		return false
	}
	return d.SHA256 == fmt.Sprintf("%x", sha256.Sum256([]byte(message)))
}

// submitEnterUntilNativeOrBusy keeps the retry guard separate from delivery proof.
func submitEnterUntilNativeOrBusy(send func() error, wake func(), accepted, busy func() (bool, error), sleep func(time.Duration)) (bool, error) {
	var delivered bool
	var probeErr error
	_, err := submitEnterAndConfirm(send, wake, func() (bool, error) {
		delivered, probeErr = accepted()
		if delivered || probeErr != nil {
			return true, nil // Stop keys on proof or lost control.
		}
		isBusy, err := busy()
		if err != nil {
			probeErr = err
			return true, nil
		}
		return isBusy, nil
	}, sleep)
	if probeErr != nil {
		return false, probeErr
	}
	// Busy prevents another submit; only the native receipt proves delivery.
	return delivered, err
}

// nativeDeliveryProbe is used only inside the per-session nudge lock. Missing
// adapters keep the stock terminal path; a present but broken adapter is an
// error, not permission to guess that delivery succeeded.
func (t *Tmux) nativeDeliveryProbe(session, message string) (func() (bool, error), error) {
	if t.providerEnv(session) != "codex" {
		return nil, nil
	}
	token, err := t.GetEnvironment(session, "GC_INSTANCE_TOKEN")
	if errors.Is(err, errEnvUnset) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading native delivery incarnation: %w", err)
	}
	if token == "" {
		return nil, nil
	}
	query := func() (codexcontrol.Target, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		reply, err := codexcontrol.Control(ctx, session, token, codexcontrol.Request{Action: "target"})
		return reply.Target, err
	}
	before, err := query()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading native delivery baseline: %w", err)
	}
	if before.ThreadID == "" {
		return nil, nil
	}
	if before.State == "unconfirmed" {
		return nil, errors.New("native delivery baseline is unconfirmed")
	}
	return func() (bool, error) {
		after, err := query()
		if err != nil {
			return false, err
		}
		return nativeDeliveryConfirmed(before, after, message), nil
	}, nil
}
