//go:build integration

package codexcontrol

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// This transport composition test owns a real loopback WebSocket listener.
// Pure command ownership and control-handler tests remain in the unit tier.
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
	b := &bridge{input: bridgeOut, routes: map[string]route{}, done: make(chan struct{}), resumePermissions: resumePermissions{approval: "never", sandbox: "danger-full-access"}}
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
	// The remote TUI omits permissions; GC transfers the explicit launch choice
	// into the native resume and rejects a server that does not confirm it.
	for _, confirmed := range []bool{true, false} {
		go func() {
			sent <- c.WriteMessage(websocket.TextMessage, []byte(`{"id":10,"method":"thread/resume","params":{"threadId":"native-exact","model":"gpt-6-luna"}}`))
		}()
		if !scan.Scan() {
			t.Fatal("missing native resume")
		}
		if err = json.Unmarshal(scan.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(request["params"]), `"approvalPolicy":"never"`) || !strings.Contains(string(request["params"]), `"sandbox":"danger-full-access"`) {
			t.Fatalf("permissions not forwarded: %s", request["params"])
		}
		if err = <-sent; err != nil {
			t.Fatal(err)
		}
		policy := "dangerFullAccess"
		if !confirmed {
			policy = "readOnly"
		}
		go func() {
			_, e := nativeOut.Write([]byte(`{"id":` + string(request["id"]) + `,"result":{"thread":{"id":"native-exact"},"approvalPolicy":"never","sandbox":{"type":"` + policy + `"}}}` + "\n"))
			sent <- e
		}()
		err = c.ReadJSON(&reply)
		if confirmed && err != nil {
			t.Fatal(err)
		}
		if !confirmed && err == nil {
			t.Fatal("accepted native permission mismatch")
		}
		if err = <-sent; err != nil {
			t.Fatal(err)
		}
	}
}
