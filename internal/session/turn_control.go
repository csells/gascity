package session

import (
	"context"
	"encoding/json"
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
		claude := providerKind(b) == "claude" || wrappedProviderFamily(b, "claude")
		if req.Action == "target" && !claude && providerKind(b) != "codex" && !wrappedProviderFamily(b, "codex") {
			target = runtime.TurnTarget{Mode: "native_interrupt", State: "unknown"}
			return nil
		}
		req.SessionKey = b.Metadata["session_key"]
		var ownership struct{ Detached, Stopped map[string]bool }
		if claude {
			req.ProviderKind = "claude"
			req.TranscriptPath = ResolveKeyedTranscriptPath(infoFromPersistedBead(b), nil)
			if raw := b.Metadata["claude_command_ownership"]; raw != "" {
				if e = json.Unmarshal([]byte(raw), &ownership); e != nil {
					return fmt.Errorf("reading retained command ownership: %w", e)
				}
			}
			if ownership.Detached == nil {
				ownership.Detached = map[string]bool{}
			}
			if ownership.Stopped == nil {
				ownership.Stopped = map[string]bool{}
			}
			req.Detached, req.Stopped = ownership.Detached, ownership.Stopped
		}
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
		if claude && (req.Action == "stop" || req.Action == "detach") {
			if target.TurnID != req.TurnID || (req.Action == "stop" && target.State != "stopped") {
				return fmt.Errorf("native command control did not confirm the requested turn")
			}
			if req.Action == "stop" {
				ownership.Stopped[req.TurnID] = true
			} else {
				ownership.Detached[req.TurnID+":"+req.ProcessID] = true
			}
			encoded, encodeErr := json.Marshal(ownership)
			if encodeErr != nil {
				return encodeErr
			}
			if e = m.store.SetMetadata(id, "claude_command_ownership", string(encoded)); e != nil {
				return fmt.Errorf("persisting command ownership: %w", e)
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
