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

	"github.com/gorilla/websocket"
)

func TestBridgeRetainsProtocolFieldsWhileMultiplexing(t *testing.T) {
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		b.serveClient(c, bridgeIn)
	}))
	defer server.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Errorf("fixture close: %v", err)
		}
	}()
	if err = c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	scan := bufio.NewScanner(nativeIn)
	sent := make(chan error, 1)
	go func() {
		sent <- c.WriteMessage(websocket.TextMessage, []byte(`{"id":7,"method":"read","params":{},"trace":"preserve"}`))
	}()
	if !scan.Scan() {
		t.Fatal("missing native request")
	}
	var request map[string]json.RawMessage
	if err = json.Unmarshal(scan.Bytes(), &request); err != nil {
		t.Fatal(err)
	}
	if string(request["trace"]) != `"preserve"` || string(request["id"]) == "7" {
		t.Fatalf("request not transparently routed: %s", scan.Bytes())
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	go func() {
		_, e := nativeOut.Write([]byte(`{"id":` + string(request["id"]) + `,"error":{"code":-1,"message":"test","data":{"reason":"fixture"}},"emittedAtMs":42}` + "\n"))
		sent <- e
	}()
	var reply map[string]json.RawMessage
	if err = c.ReadJSON(&reply); err != nil {
		t.Fatal(err)
	}
	if string(reply["id"]) != "7" || string(reply["emittedAtMs"]) != "42" || !strings.Contains(string(reply["error"]), `"data"`) {
		t.Fatalf("response fields lost: %v", reply)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	// A successful native startup reply, not directory discovery, binds this
	// incarnation to its exact conversation before its first user turn.
	go func() {
		sent <- c.WriteMessage(websocket.TextMessage, []byte(`{"id":8,"method":"thread/start","params":{}}`))
	}()
	if !scan.Scan() {
		t.Fatal("missing native startup request")
	}
	if err = json.Unmarshal(scan.Bytes(), &request); err != nil {
		t.Fatal(err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	go func() {
		_, e := nativeOut.Write([]byte(`{"id":` + string(request["id"]) + `,"result":{"thread":{"id":"native-exact"}}}` + "\n"))
		sent <- e
	}()
	if err = c.ReadJSON(&reply); err != nil {
		t.Fatal(err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	b.serveControl(rec, httptest.NewRequest("POST", "/control", strings.NewReader(`{"action":"target"}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"thread_id":"native-exact"`) {
		t.Fatalf("native startup identity was not retained: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	b.serveControl(rec, httptest.NewRequest("POST", "/control", strings.NewReader(`{"action":"target","thread_id":"foreign"}`)))
	if rec.Code != 409 {
		t.Fatalf("foreign conversation accepted: %d %s", rec.Code, rec.Body.String())
	}
	// Codex uses ephemeral native threads for title/structured requests. They
	// must survive without replacing the user conversation's identity.
	go func() {
		sent <- c.WriteMessage(websocket.TextMessage, []byte(`{"id":9,"method":"thread/start","params":{"ephemeral":true}}`))
	}()
	if !scan.Scan() {
		t.Fatal("missing ephemeral start")
	}
	if err = json.Unmarshal(scan.Bytes(), &request); err != nil {
		t.Fatal(err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	go func() {
		_, e := nativeOut.Write([]byte(`{"id":` + string(request["id"]) + `,"result":{"thread":{"id":"ephemeral-title","ephemeral":true}}}` + "\n"))
		sent <- e
	}()
	if err = c.ReadJSON(&reply); err != nil {
		t.Fatalf("ephemeral thread killed main connection: %v", err)
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	b.serveControl(rec, httptest.NewRequest("POST", "/control", strings.NewReader(`{"action":"target"}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"thread_id":"native-exact"`) {
		t.Fatal("ephemeral start rebound main conversation")
	}
	// Server-originated approvals retain their ID so the TUI's answer routes back.
	go func() {
		_, e := nativeOut.Write([]byte(`{"id":"approval","method":"item/commandExecution/requestApproval","params":{}}` + "\n"))
		sent <- e
	}()
	if err = c.ReadJSON(&reply); err != nil {
		t.Fatal(err)
	}
	if string(reply["id"]) != `"approval"` {
		t.Fatal("approval identity rewritten")
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	go func() {
		sent <- c.WriteMessage(websocket.TextMessage, []byte(`{"id":"approval","result":{"decision":"decline"}}`))
	}()
	if !scan.Scan() || !strings.Contains(scan.Text(), `"id":"approval"`) {
		t.Fatal("approval reply lost")
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	// A second client must not steal the first stream's native reader.
	second, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Errorf("fixture close: %v", err)
		}
	}()
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = second.ReadMessage(); err == nil {
		t.Fatal("second client accepted")
	}
	go func() {
		_, e := nativeOut.Write([]byte(`{"method":"notice","params":{},"emittedAtMs":43}` + "\n"))
		sent <- e
	}()
	if err = c.ReadJSON(&reply); err != nil {
		t.Fatal(err)
	}
	if string(reply["method"]) != `"notice"` {
		t.Fatal("first connection lost ownership")
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
}

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
