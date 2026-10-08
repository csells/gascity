package runtime

import "context"

// TurnTarget is an opaque cancellation target from an authoritative runtime.
type TurnTarget struct {
	// SessionKey is private runtime proof for the session owner, not public API.
	SessionKey string `json:"-"`
	Mode       string `json:"mode" enum:"owned_commands,native_interrupt"`
	TurnID     string `json:"turn_id,omitempty"`
	State      string `json:"state"`
}

// TurnControlRequest preserves native conversation and turn ownership across the runtime boundary.
type TurnControlRequest struct{ Action, SessionKey, TurnID, ProcessID string }

// TurnControlProvider controls commands without scraping a terminal or walking OS processes.
type TurnControlProvider interface {
	ControlTurn(context.Context, string, TurnControlRequest) (TurnTarget, error)
}
