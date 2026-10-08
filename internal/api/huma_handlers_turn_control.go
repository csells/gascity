package api

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// SessionTurnControlInput binds every command mutation to a caller-selected turn.
type SessionTurnControlInput struct {
	CityScope
	ID   string `path:"id" doc:"Gas City session ID."`
	Body struct {
		TurnID    string `json:"turn_id" minLength:"1" doc:"Opaque turn identity returned by the runtime target endpoint. A stale target cannot stop new work."`
		ProcessID string `json:"process_id,omitempty" doc:"Native process identity; required only to explicitly detach a command."`
	}
}

// SessionTurnTargetOutput is the typed native-control target.
type SessionTurnTargetOutput struct{ Body runtime.TurnTarget }

func (s *Server) humaHandleSessionTurnTarget(ctx context.Context, input *SessionIDInput) (*SessionTurnTargetOutput, error) {
	return s.controlSessionTurn(ctx, input.ID, runtime.TurnControlRequest{Action: "target"})
}

func (s *Server) humaHandleSessionTurnStop(ctx context.Context, input *SessionTurnControlInput) (*SessionTurnTargetOutput, error) {
	return s.controlSessionTurn(ctx, input.ID, runtime.TurnControlRequest{Action: "stop", TurnID: input.Body.TurnID})
}

func (s *Server) humaHandleSessionTurnDetach(ctx context.Context, input *SessionTurnControlInput) (*SessionTurnTargetOutput, error) {
	if input.Body.ProcessID == "" {
		return nil, apierr.InvalidRequest.Msg("process_id is required")
	}
	return s.controlSessionTurn(ctx, input.ID, runtime.TurnControlRequest{Action: "detach", TurnID: input.Body.TurnID, ProcessID: input.Body.ProcessID})
}

func (s *Server) controlSessionTurn(ctx context.Context, id string, req runtime.TurnControlRequest) (*SessionTurnTargetOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	resolved, err := s.resolveSessionIDWithConfig(store.Store, id)
	if err != nil {
		return nil, humaResolveError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	handle, err := s.workerHandleForSession(store.Store, resolved)
	if err != nil {
		return nil, humaSessionManagerError(err)
	}
	control, ok := handle.(worker.TurnControlHandle)
	if !ok {
		return nil, apierr.SessionConflict.Msg("worker does not support owned-turn control")
	}
	target, err := control.ControlTurn(ctx, req)
	if err != nil {
		return nil, apierr.SessionConflict.Msg(err.Error())
	}
	return &SessionTurnTargetOutput{Body: target}, nil
}
