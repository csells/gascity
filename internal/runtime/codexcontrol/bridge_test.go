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

func TestResumePermissionsTravelThroughNativeAPI(t *testing.T) {
	for _, tt := range []struct {
		name              string
		args              []string
		approval, sandbox string
	}{
		{"unrestricted", []string{"resume", "--dangerously-bypass-approvals-and-sandbox", "thread", "--model", "gpt-6-luna"}, "never", "danger-full-access"},
		{"restricted", []string{"resume", "-a", "untrusted", "--sandbox=read-only", "thread"}, "untrusted", "read-only"},
		{"workspace", []string{"-s", "workspace-write", "resume", "thread", "-a", "on-request"}, "on-request", "workspace-write"},
		{"native bypass precedence", []string{"resume", "--yolo", "-a=untrusted", "-s", "read-only", "thread"}, "never", "danger-full-access"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args, permissions, err := prepareResumePermissions(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if permissions.approval != tt.approval || permissions.sandbox != tt.sandbox {
				t.Fatalf("lost requested permissions: %+v", permissions)
			}
			for _, arg := range args {
				if strings.Contains(arg, "sandbox") || strings.Contains(arg, "approval") || arg == "--full-auto" {
					t.Fatalf("remote TUI still receives permission flag: %v", args)
				}
			}
			raw, err := permissions.apply(json.RawMessage(`{"threadId":"thread","model":"gpt-6-luna","config":{"model_reasoning_effort":"medium"},"extension":true}`))
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ThreadID  string            `json:"threadId"`
				Approval  string            `json:"approvalPolicy"`
				Sandbox   string            `json:"sandbox"`
				Extension bool              `json:"extension"`
				Config    map[string]string `json:"config"`
			}
			if err = json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.ThreadID != "thread" || got.Approval != tt.approval || got.Sandbox != tt.sandbox || !got.Extension || got.Config["model_reasoning_effort"] != "medium" {
				t.Fatalf("resume changed semantics: %s", raw)
			}
			wireSandbox := map[string]string{"danger-full-access": "dangerFullAccess", "read-only": "readOnly", "workspace-write": "workspaceWrite"}[tt.sandbox]
			result := json.RawMessage(`{"approvalPolicy":"` + tt.approval + `","sandbox":{"type":"` + wireSandbox + `"}}`)
			if err = permissions.verify(result); err != nil {
				t.Fatal(err)
			}
			if err = permissions.verify(json.RawMessage(`{"approvalPolicy":"on-request","sandbox":{"type":"unknown"}}`)); err == nil {
				t.Fatal("accepted different native permissions")
			}
			if err = permissions.verify(json.RawMessage(`{}`)); err == nil {
				t.Fatal("accepted unverified permissions")
			}
		})
	}
}

func TestResumePermissionsPreserveUnrelatedArgumentsAndRejectAmbiguity(t *testing.T) {
	for _, args := range [][]string{
		{"--model", "resume", "--dangerously-bypass-approvals-and-sandbox"},
		{"--", "resume"},
		{"resume", "thread", "--", "--sandbox=read-only"},
	} {
		got, permissions, err := prepareResumePermissions(args)
		if err != nil || strings.Join(got, "\x00") != strings.Join(args, "\x00") || permissions != (resumePermissions{}) {
			t.Fatalf("changed non-permission arguments: %v %+v %v", got, permissions, err)
		}
	}
	for _, args := range [][]string{
		{"resume", "thread", "--sandbox"},
		{"resume", "thread", "--sandbox=typo"},
		{"resume", "thread", "--ask-for-approval=typo"},
		{"resume", "thread", "--full-auto", "--yolo"},
		{"resume", "thread", "-c", "approval_policy=\"never\""},
		{"resume", "thread", "--add-dir", "/tmp"},
		{"resume", "thread", "--profile", "custom"},
	} {
		if _, _, err := prepareResumePermissions(args); err == nil {
			t.Fatalf("silently accepted unsupported/ambiguous permissions: %v", args)
		}
	}
}
