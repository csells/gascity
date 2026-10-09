package codexcontrol

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNativeDisconnectFailsPendingControl(t *testing.T) {
	input, output := io.Pipe()
	defer func() {
		if err := input.Close(); err != nil {
			t.Errorf("fixture close: %v", err)
		}
	}()
	defer func() {
		if err := output.Close(); err != nil {
			t.Errorf("fixture close: %v", err)
		}
	}()
	b := &bridge{input: output, routes: map[string]route{}, done: make(chan struct{})}
	b.controller = New(b)
	result := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { result <- b.Call(ctx, "turn/interrupt", struct{}{}, &struct{}{}) }()
	scan := bufio.NewScanner(input)
	if !scan.Scan() {
		t.Fatal("request missing")
	}
	b.fail(io.EOF)
	if err := <-result; err == nil {
		t.Fatal("native disconnect reported success")
	}
	if b.controller.Target("thread").State != "unconfirmed" {
		t.Fatal("lost native stream did not fence ownership")
	}
}

func TestLateControlReplyDoesNotDestroyNativeConversation(t *testing.T) {
	nativeIn, bridgeOut := io.Pipe()
	bridgeIn, nativeOut := io.Pipe()
	defer func() {
		if err := nativeIn.Close(); err != nil {
			t.Errorf("fixture close: %v", err)
		}
	}()
	defer func() {
		if err := bridgeOut.Close(); err != nil {
			t.Errorf("fixture close: %v", err)
		}
	}()
	defer func() {
		if err := bridgeIn.Close(); err != nil {
			t.Errorf("fixture close: %v", err)
		}
	}()
	defer func() {
		if err := nativeOut.Close(); err != nil {
			t.Errorf("fixture close: %v", err)
		}
	}()
	b := &bridge{input: bridgeOut, routes: map[string]route{}, done: make(chan struct{})}
	b.controller = New(b)
	go b.readNative(bridgeIn)
	scan := bufio.NewScanner(nativeIn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- b.Call(ctx, "stop", struct{}{}, &struct{}{}) }()
	if !scan.Scan() {
		t.Fatal("request missing")
	}
	var request envelope
	if err := json.Unmarshal(scan.Bytes(), &request); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel returned %v", err)
	}
	late := make(chan error, 1)
	go func() {
		_, err := nativeOut.Write([]byte(`{"id":` + string(request.ID) + `,"result":{}}` + "\n"))
		late <- err
	}()
	if err := <-late; err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	second := make(chan error, 1)
	go func() { second <- b.Call(ctx2, "read", struct{}{}, &struct{}{}) }()
	if !scan.Scan() {
		t.Fatal("next request missing")
	}
	if err := json.Unmarshal(scan.Bytes(), &request); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := nativeOut.Write([]byte(`{"id":` + string(request.ID) + `,"result":{}}` + "\n"))
		late <- err
	}()
	if err := <-second; err != nil {
		t.Fatalf("late reply killed control stream: %v", err)
	}
	if err := <-late; err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedStopResponseNamesOldTurnWithoutClaimingNewTurnStopped(t *testing.T) {
	c, _ := fixture(t)
	if err := c.Stop(context.Background(), "thread", "turn"); err != nil {
		t.Fatal(err)
	}
	if err := c.Observe("turn/started", json.RawMessage(`{"threadId":"thread","turn":{"id":"new","status":"inProgress"}}`)); err != nil {
		t.Fatal(err)
	}
	b := &bridge{controller: c, nativeThread: "thread"}
	rec := httptest.NewRecorder()
	b.serveControl(rec, httptest.NewRequest("POST", "/control", strings.NewReader(`{"action":"stop","thread_id":"thread","turn_id":"turn"}`)))
	var reply Reply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || reply.Target.TurnID != "turn" || reply.Target.State != "stopped" {
		t.Fatalf("misidentified stopped turn: %s", rec.Body.String())
	}
	if c.Target("thread").TurnID != "new" || c.Target("thread").State != "inProgress" {
		t.Fatal("repeat stop changed new turn")
	}
}

func TestInitialTargetCannotGuessAnExistingNativeTurn(t *testing.T) {
	c := New(nil)
	b := &bridge{controller: c}
	probe := func() *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		b.serveControl(r, httptest.NewRequest("POST", "/control", strings.NewReader(`{"action":"target"}`)))
		return r
	}
	r := probe()
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"state":"initializing"`) {
		t.Fatalf("fresh native baseline refused: %d %s", r.Code, r.Body.String())
	}
	if err := c.Observe("turn/started", json.RawMessage(`{"threadId":"native","turn":{"id":"existing","status":"inProgress"}}`)); err != nil {
		t.Fatal(err)
	}
	if r = probe(); r.Code != 409 {
		t.Fatalf("unknown identity guessed an existing turn: %d %s", r.Code, r.Body.String())
	}
}
