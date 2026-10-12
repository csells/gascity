package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// neverCommandableState defers session starts to a controller that never makes
// the session commandable, as when the start outlives the create wait.
type neverCommandableState struct {
	*fakeState
}

func (s *neverCommandableState) WaitForSessionCommandable(_ context.Context, _ string) (session.Info, error) {
	return session.Info{}, errors.New("session did not become commandable")
}

// TestSessionCreateFailureAfterCreateNamesTheSession pins the outcome of a
// create whose session bead was written but which then failed waiting to
// become commandable. The session exists, so request.failed must say which one:
// otherwise the client cannot find or clean it up, and a retry creates a second.
func TestSessionCreateFailureAfterCreateNamesTheSession(t *testing.T) {
	fs := newSessionFakeState(t)
	state := &neverCommandableState{fakeState: fs}
	h := newTestCityHandlerWith(t, state, New(state))

	body := `{"kind":"agent","name":"myrig/worker","alias":"late","async":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)

	failed := waitForRequestFailed(t, fs.eventProv, accepted.RequestID, testEventTimeout)

	created, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("session beads = %d, want the 1 the create wrote", len(created))
	}
	if failed.SessionID != created[0].ID {
		t.Fatalf("request.failed session_id = %q, want %q (the session the failed create left behind); payload: %+v", failed.SessionID, created[0].ID, failed)
	}
}
