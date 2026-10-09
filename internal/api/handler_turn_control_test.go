package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestOwnedStopRefusesUnsupportedRuntimeAndMissingTurn(t *testing.T) {
	fs := newSessionFakeState(t)
	h := newTestCityHandler(t, fs)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "owned-stop")
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/stop-commands", `{}`, http.StatusUnprocessableEntity},
		{"/stop-commands", `{"turn_id":"old-turn"}`, http.StatusConflict},
		{"/detach-command", `{"turn_id":"turn"}`, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+info.ID+tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.status {
			t.Fatalf("%s: got %d: %s", tc.path, rec.Code, rec.Body.String())
		}
	}
	for _, call := range fs.sp.Calls {
		if call.Method == "Interrupt" || call.Method == "SendKeys" || call.Method == "Stop" {
			t.Fatalf("unsupported owned Stop performed legacy mutation: %v", call)
		}
	}
}

type turnControlProvider struct {
	*runtime.Fake
	request       runtime.TurnControlRequest
	nativeKey     string
	responseState string
}

func (p *turnControlProvider) ControlTurn(_ context.Context, _ string, r runtime.TurnControlRequest) (runtime.TurnTarget, error) {
	p.request = r
	if r.Action == "target" {
		return runtime.TurnTarget{Mode: "owned_commands", SessionKey: p.nativeKey, TurnID: "turn-expected", State: "inProgress"}, nil
	}
	if r.TurnID != "turn-expected" {
		return runtime.TurnTarget{}, errors.New("stale native turn")
	}
	state := p.responseState
	if state == "" {
		state = "interrupted"
	}
	return runtime.TurnTarget{Mode: "owned_commands", SessionKey: p.nativeKey, TurnID: r.TurnID, State: state}, nil
}

func TestClaudeOwnedStopPersistsOnlyConfirmedTurn(t *testing.T) {
	fs := newSessionFakeState(t)
	p := &turnControlProvider{Fake: fs.sp, nativeKey: "native-claude"}
	h := newTestCityHandler(t, &stateWithSessionProvider{fakeState: fs, provider: p})
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "claude-owned")
	for key, value := range map[string]string{"provider": "claude", "builtin_ancestor": "claude", "session_key": "native-claude"} {
		if err := fs.cityBeadStore.SetMetadata(info.ID, key, value); err != nil {
			t.Fatal(err)
		}
	}
	baseline := httptest.NewRecorder()
	h.ServeHTTP(baseline, httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/turn-target", nil))
	if baseline.Code != http.StatusOK || p.request.ProviderKind != "claude" || !strings.Contains(baseline.Body.String(), `"mode":"owned_commands"`) {
		t.Fatalf("Claude bypassed ownership: %d %s", baseline.Code, baseline.Body.String())
	}
	for _, state := range []string{"interrupted", "stopped"} {
		p.responseState = state
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+info.ID+"/stop-commands", strings.NewReader(`{"turn_id":"turn-expected"}`)))
		want := http.StatusConflict
		if state == "stopped" {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Fatalf("%s: %d %s", state, rec.Code, rec.Body.String())
		}
		if state == "interrupted" {
			pending := httptest.NewRecorder()
			h.ServeHTTP(pending, httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/turn-target", nil))
			if pending.Code != http.StatusOK || !strings.Contains(pending.Body.String(), `"state":"unconfirmed"`) {
				t.Fatalf("unconfirmed explicit Stop lost its durable fence: %d %s", pending.Code, pending.Body.String())
			}
		}
	}
	// A subsequent request receives the persisted idempotence fence.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/turn-target", nil))
	if rec.Code != http.StatusOK || !p.request.Stopped["turn-expected"] {
		t.Fatalf("confirmed turn fence was lost: %d %+v", rec.Code, p.request)
	}
}

func TestOwnedStopCarriesPersistedIdentityAndRejectsStaleTurn(t *testing.T) {
	fs := newSessionFakeState(t)
	p := &turnControlProvider{Fake: fs.sp}
	h := newTestCityHandler(t, &stateWithSessionProvider{fakeState: fs, provider: p})
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "native-owned-stop")
	if err := fs.cityBeadStore.SetMetadata(info.ID, "session_key", "native-conversation"); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []string{"stale", "turn-expected"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+info.ID+"/stop-commands", strings.NewReader(`{"turn_id":"`+turn+`"}`)))
		expected := http.StatusOK
		if turn == "stale" {
			expected = http.StatusConflict
		}
		if rec.Code != expected {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if p.request.SessionKey != "native-conversation" || p.request.TurnID != turn || p.request.Action != "stop" {
			t.Fatalf("wrong ownership %v", p.request)
		}
	}
}

func TestOwnedStopTargetsBuiltinCodexInsteadOfLegacyInterrupt(t *testing.T) {
	fs := newSessionFakeState(t)
	p := &turnControlProvider{Fake: fs.sp}
	h := newTestCityHandler(t, &stateWithSessionProvider{fakeState: fs, provider: p})
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "builtin-codex-stop")
	for key, value := range map[string]string{"provider": "codex", "builtin_ancestor": "codex", "session_key": "native-conversation"} {
		if err := fs.cityBeadStore.SetMetadata(info.ID, key, value); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/turn-target", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"mode":"owned_commands"`) {
		t.Fatalf("builtin Codex bypassed owned Stop: %d %s", rec.Code, rec.Body.String())
	}
	if p.request.SessionKey != "native-conversation" || p.request.Action != "target" {
		t.Fatalf("target did not reach native owner: %v", p.request)
	}
}

func TestOwnedStopBaselineBeforeFirstNativeTurn(t *testing.T) {
	fs := newSessionFakeState(t)
	p := &turnControlProvider{Fake: fs.sp}
	h := newTestCityHandler(t, &stateWithSessionProvider{fakeState: fs, provider: p})
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "new-codex-stop")
	if err := fs.cityBeadStore.SetMetadata(info.ID, "provider", "codex"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/turn-target", nil))
	if rec.Code != http.StatusOK || p.request.Action != "target" || p.request.SessionKey != "" {
		t.Fatalf("first-turn baseline must reach the owning runtime: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/")+info.ID+"/stop-commands", strings.NewReader(`{"turn_id":"turn-expected"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("missing native identity permitted mutation: %d", rec.Code)
	}
}

func TestOwnedTargetPersistsAuthoritativeNativeIdentity(t *testing.T) {
	fs := newSessionFakeState(t)
	p := &turnControlProvider{Fake: fs.sp, nativeKey: "native-exact"}
	h := newTestCityHandler(t, &stateWithSessionProvider{fakeState: fs, provider: p})
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "native-binding")
	if err := fs.cityBeadStore.SetMetadata(info.ID, "provider", "codex"); err != nil {
		t.Fatal(err)
	}
	probe := func() *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/turn-target", nil))
		return r
	}
	r := probe()
	if r.Code != 200 {
		t.Fatalf("binding failed: %d %s", r.Code, r.Body.String())
	}
	bead, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bead.Metadata["session_key"] != "native-exact" {
		t.Fatalf("native identity not persisted: %q", bead.Metadata["session_key"])
	}
	if strings.Contains(r.Body.String(), "native-exact") {
		t.Fatal("internal identity leaked into turn target API")
	}
	p.nativeKey = "foreign"
	r = probe()
	if r.Code != 409 {
		t.Fatalf("foreign native identity accepted: %d %s", r.Code, r.Body.String())
	}
	bead, err = fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bead.Metadata["session_key"] != "native-exact" {
		t.Fatal("conversation was rebound")
	}
}

func TestOwnedHistoryBindsNativeIdentityBeforeTranscriptRead(t *testing.T) {
	fs := newSessionFakeState(t)
	p := &turnControlProvider{Fake: fs.sp, nativeKey: "native-exact"}
	h := newTestCityHandler(t, &stateWithSessionProvider{fakeState: fs, provider: p})
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "native-history")
	if err := fs.cityBeadStore.SetMetadata(info.ID, "provider", "codex"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured", nil))
	b, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["session_key"] != "native-exact" {
		t.Fatalf("history lookup did not bind authoritative identity: status=%d key=%q", rec.Code, b.Metadata["session_key"])
	}
}
