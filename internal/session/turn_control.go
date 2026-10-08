package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ControlTurn reaches an ownership-aware native runtime without changing session identity.
func (m *Manager) ControlTurn(ctx context.Context, id string, req runtime.TurnControlRequest) (target runtime.TurnTarget, err error) {
	err = withSessionMutationLock(id, func() error {
		b, name, e := m.sessionBead(id)
		if e != nil {
			return e
		}
		if req.Action == "target" && providerKind(b) != "codex" && !wrappedProviderFamily(b, "codex") {
			target = runtime.TurnTarget{Mode: "native_interrupt", State: "unknown"}
			return nil
		}
		req.SessionKey = b.Metadata["session_key"]
		if req.SessionKey == "" && req.Action != "target" {
			return fmt.Errorf("%w: native conversation identity missing", runtime.ErrInteractionUnsupported)
		}
		leaf, _, known := runtime.ResolveBackend(m.sp, name)
		if !known {
			return fmt.Errorf("%w: runtime route unknown", runtime.ErrInteractionUnsupported)
		}
		control, ok := leaf.(runtime.TurnControlProvider)
		if !ok {
			return fmt.Errorf("%w: owned command control unavailable", runtime.ErrInteractionUnsupported)
		}
		target, e = control.ControlTurn(ctx, name, req)
		if e != nil {
			return e
		}
		if target.SessionKey != "" {
			if req.SessionKey != "" && req.SessionKey != target.SessionKey {
				return fmt.Errorf("native runtime changed conversation identity")
			}
			if req.SessionKey == "" {
				if e = m.store.SetMetadata(id, "session_key", target.SessionKey); e != nil {
					return fmt.Errorf("persisting native runtime identity: %w", e)
				}
			}
		}
		return nil
	})
	return target, err
}

// RefreshNativeIdentity learns a key only from the exact retained runtime. It
// leaves legacy runtimes without this optional capability on their hook path.
func (m *Manager) RefreshNativeIdentity(ctx context.Context, id string) error {
	b, _, err := m.loadSessionBead(id, true)
	if err != nil {
		return err
	}
	if b.Status == "closed" || b.Metadata["session_key"] != "" || (providerKind(b) != "codex" && !wrappedProviderFamily(b, "codex")) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err = m.ControlTurn(ctx, id, runtime.TurnControlRequest{Action: "target"})
	if errors.Is(err, runtime.ErrInteractionUnsupported) {
		return nil
	}
	return err
}
