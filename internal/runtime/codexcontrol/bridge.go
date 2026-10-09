package codexcontrol

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type envelope struct {
	ID     json.RawMessage            `json:"id,omitempty"`
	Method string                     `json:"method,omitempty"`
	Params json.RawMessage            `json:"params,omitempty"`
	Result json.RawMessage            `json:"result,omitempty"`
	Error  *rpcError                  `json:"error,omitempty"`
	Extra  map[string]json.RawMessage `json:"-"`
}
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Preserve optional native protocol fields when rewriting only request IDs.
func (e *envelope) UnmarshalJSON(raw []byte) error {
	type fields envelope
	var value fields
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extra); err != nil {
		return err
	}
	for _, key := range []string{"id", "method", "params", "result", "error"} {
		delete(extra, key)
	}
	*e = envelope(value)
	e.Extra = extra
	return nil
}

func (e envelope) MarshalJSON() ([]byte, error) {
	type fields envelope
	raw, err := json.Marshal(fields(e))
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err = json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	for key, value := range e.Extra {
		if _, exists := all[key]; !exists {
			all[key] = value
		}
	}
	return json.Marshal(all)
}

type response struct {
	result json.RawMessage
	err    error
}
type route struct {
	method          string
	requestedThread string
	ephemeral       bool
	original        json.RawMessage
	reply           chan response
	finish          func()
}
type bridge struct {
	resumePermissions resumePermissions
	failure           error
	nativeThread      string
	input             io.WriteCloser
	writeMu           sync.Mutex
	mu                sync.Mutex
	routes            map[string]route
	client            *websocket.Conn
	clientMu          sync.Mutex
	sequence          atomic.Uint64
	controller        *Controller
	done              chan struct{}
	once              sync.Once
}

func (b *bridge) send(e envelope) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return json.NewEncoder(b.input).Encode(e)
}

func (b *bridge) next() json.RawMessage {
	return json.RawMessage(strconv.Quote(fmt.Sprintf("gc-control-%d", b.sequence.Add(1))))
}

func (b *bridge) forward(e envelope) error {
	b.clientMu.Lock()
	defer b.clientMu.Unlock()
	b.mu.Lock()
	c := b.client
	b.mu.Unlock()
	if c == nil {
		return errors.New("native client disconnected")
	}
	if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return c.WriteJSON(e)
}

func (b *bridge) fail(err error) {
	b.once.Do(func() {
		b.mu.Lock()
		b.failure = err
		b.mu.Unlock()
		b.controller.Fail(err)
		close(b.done)
		b.mu.Lock()
		defer b.mu.Unlock()
		for id, r := range b.routes {
			if r.reply != nil {
				r.reply <- response{err: err}
			}
			if r.finish != nil {
				r.finish()
			}
			delete(b.routes, id)
		}
	})
}

// Call sends an internal native request without exposing it to the terminal client.
func (b *bridge) Call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := b.next()
	ch := make(chan response, 1)
	b.mu.Lock()
	b.routes[string(id)] = route{reply: ch}
	b.mu.Unlock()
	// Keep the route after caller cancellation: a late native reply must be
	// consumed without being mistaken for an unsolicited protocol response.
	if err = b.send(envelope{ID: id, Method: method, Params: raw}); err != nil {
		b.mu.Lock()
		delete(b.routes, string(id))
		b.mu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return errors.New("native control stream lost")
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		return json.Unmarshal(r.result, result)
	}
}

func (b *bridge) readNative(output io.Reader) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	for scanner.Scan() {
		var e envelope
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			b.fail(err)
			return
		}
		if len(e.ID) > 0 && e.Method == "" {
			b.mu.Lock()
			r, ok := b.routes[string(e.ID)]
			if ok {
				delete(b.routes, string(e.ID))
			}
			b.mu.Unlock()
			if !ok {
				b.fail(errors.New("unmatched native response"))
				return
			}
			if e.Error == nil && !r.ephemeral && (r.method == "thread/start" || r.method == "thread/resume") {
				var result struct {
					Thread struct {
						ID string `json:"id"`
					} `json:"thread"`
				}
				if err := json.Unmarshal(e.Result, &result); err != nil {
					b.fail(err)
					return
				}
				if r.method == "thread/resume" {
					if err := b.resumePermissions.verify(e.Result); err != nil {
						b.fail(err)
						return
					}
				}
				if err := b.bindNativeThread(result.Thread.ID, r.requestedThread); err != nil {
					b.fail(err)
					return
				}
			}
			if r.finish != nil {
				r.finish()
			}
			if r.reply != nil {
				var err error
				if e.Error != nil {
					err = fmt.Errorf("native %d: %s", e.Error.Code, e.Error.Message)
				}
				r.reply <- response{result: e.Result, err: err}
				continue
			}
			e.ID = r.original
		}
		if e.Method != "" && len(e.ID) == 0 {
			if err := b.controller.Observe(e.Method, e.Params); err != nil {
				b.fail(err)
				return
			}
		}
		if err := b.forward(e); err != nil {
			b.fail(err)
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = errors.New("native app-server closed")
	}
	b.fail(err)
}

func (b *bridge) serveClient(c *websocket.Conn, output io.Reader) {
	b.mu.Lock()
	if b.client != nil {
		b.mu.Unlock()
		_ = c.Close()
		return
	}
	b.client = c
	b.mu.Unlock()
	go b.readNative(output)
	defer func() { _ = c.Close(); b.fail(errors.New("native terminal connection closed")) }()
	c.SetReadLimit(16 << 20)
	for {
		var e envelope
		if err := c.ReadJSON(&e); err != nil {
			b.fail(err)
			return
		}
		if e.Method == "thread/resume" && len(e.ID) > 0 {
			params, err := b.resumePermissions.apply(e.Params)
			if err != nil {
				b.fail(err)
				return
			}
			e.Params = params
		}
		if e.Method != "" && len(e.ID) > 0 {
			var finish func()
			if e.Method == "turn/start" || e.Method == "turn/steer" {
				var params struct {
					ThreadID string `json:"threadId"`
				}
				if err := json.Unmarshal(e.Params, &params); err != nil {
					b.fail(err)
					return
				}
				var err error
				finish, err = b.controller.BeginStart(params.ThreadID)
				if err != nil {
					if err = b.forward(envelope{ID: e.ID, Error: &rpcError{Code: -32000, Message: err.Error()}}); err != nil {
						b.fail(err)
						return
					}
					continue
				}
			}
			original := e.ID
			e.ID = b.next()
			b.mu.Lock()
			var requested struct {
				ThreadID  string `json:"threadId"`
				Ephemeral bool   `json:"ephemeral"`
			}
			if e.Method == "thread/resume" || e.Method == "thread/start" {
				if err := json.Unmarshal(e.Params, &requested); err != nil {
					b.mu.Unlock()
					b.fail(err)
					return
				}
			}
			b.routes[string(e.ID)] = route{original: original, finish: finish, method: e.Method, requestedThread: requested.ThreadID, ephemeral: requested.Ephemeral}
			b.mu.Unlock()
		}
		if err := b.send(e); err != nil {
			b.fail(err)
			return
		}
	}
}

// bindNativeThread accepts only a correlated native start/resume reply. Events
// from subagents cannot retarget this incarnation's conversation.
func (b *bridge) bindNativeThread(id, requested string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == "" || (requested != "" && requested != id) || (b.nativeThread != "" && b.nativeThread != id) {
		return errors.New("native start/resume changed conversation identity")
	}
	b.nativeThread = id
	return nil
}

// Request is the private runtime-control protocol. The public wire is the supervisor API.
type Request struct {
	Action    string `json:"action"`
	ThreadID  string `json:"thread_id"`
	TurnID    string `json:"turn_id,omitempty"`
	ProcessID string `json:"process_id,omitempty"`
}

// Reply reports either an authoritative target/result or a visible failure.
type Reply struct {
	Target Target `json:"target"`
	Error  string `json:"error,omitempty"`
}

func (b *bridge) serveControl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	var err error
	if r.Method != "POST" {
		err = errors.New("POST required")
	} else {
		err = dec.Decode(&req)
	}
	var initial *Target
	if err == nil {
		b.mu.Lock()
		bound := b.nativeThread
		b.mu.Unlock()
		switch {
		case req.ThreadID != "" && req.ThreadID != bound:
			err = errors.New("native conversation identity does not match this runtime")
		case req.ThreadID == "" && req.Action != "target":
			err = errors.New("native thread identity required")
		case bound != "":
			req.ThreadID = bound
		default:
			var target Target
			target, err = b.controller.InitialTarget()
			initial = &target
		}
	}
	if err == nil && initial == nil {
		switch req.Action {
		case "target":
		case "stop":
			err = b.controller.Stop(r.Context(), req.ThreadID, req.TurnID)
		case "detach":
			err = b.controller.Detach(req.ThreadID, req.TurnID, req.ProcessID)
		default:
			err = errors.New("unknown native control action")
		}
	}

	result := Reply{}
	if initial != nil {
		result.Target = *initial
	} else if req.ThreadID != "" {
		result.Target = b.controller.Target(req.ThreadID)
	}
	if err == nil && req.Action == "stop" {
		result.Target = Target{ThreadID: req.ThreadID, TurnID: req.TurnID, State: "stopped"}
	}
	if err != nil {
		result.Error = err.Error()
		w.WriteHeader(http.StatusConflict)
	}
	if err = json.NewEncoder(w).Encode(result); err != nil {
		b.controller.Fail(fmt.Errorf("writing native control response: %w", err))
	}
}

// SocketPaths binds control to an exact GC runtime incarnation, not an agent name alone.
func SocketPaths(name, token string) (string, string, error) {
	if name == "" || token == "" {
		return "", "", errors.New("GC session name and incarnation token are required")
	}
	sum := sha256.Sum256([]byte(name + "\x00" + token))
	base := filepath.Join(os.TempDir(), "gc-codex-"+strconv.Itoa(os.Getuid()), hex.EncodeToString(sum[:16]))
	return base + ".rpc", base + ".control", nil
}

// Control reaches only a GC-owned incarnation's private socket.
func Control(ctx context.Context, name, token string, req Request) (result Reply, controlErr error) {
	_, path, err := SocketPaths(name, token)
	if err != nil {
		return result, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	data, err := json.Marshal(req)
	if err != nil {
		return result, err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", "http://runtime/control", bytes.NewReader(data))
	if err != nil {
		return result, err
	}
	resp, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return result, fmt.Errorf("native command control unavailable: %w", err)
	}
	defer closeWithError(&controlErr, "closing native control response", resp.Body)
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&result); err != nil {
		return result, err
	}
	if resp.StatusCode != http.StatusOK || result.Error != "" {
		return result, fmt.Errorf("native command control: %s", result.Error)
	}
	return result, nil
}

// Run launches a native Codex app-server and its TUI with retained command ownership.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (runErr error) {
	args, permissions, err := prepareResumePermissions(args)
	if err != nil {
		return err
	}
	rpc, control, err := SocketPaths(os.Getenv("GC_SESSION_NAME"), os.Getenv("GC_INSTANCE_TOKEN"))
	if err != nil {
		return err
	}
	dir := filepath.Dir(rpc)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("unsafe native-control socket directory")
	}
	rl, cl, err := listenNativeSockets(rpc, control)
	if err != nil {
		return err
	}
	defer closeWithError(&runErr, "closing native RPC listener", rl)
	defer closeWithError(&runErr, "closing native control listener", cl)
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	native := exec.CommandContext(childCtx, "codex", "app-server", "--stdio")
	native.Stderr = stderr
	// Remote TUI startup does not apply --cd to its server process.
	for i, arg := range args {
		if (arg == "-C" || arg == "--cd") && i+1 < len(args) {
			native.Dir = args[i+1]
		}
		if strings.HasPrefix(arg, "--cd=") {
			native.Dir = strings.TrimPrefix(arg, "--cd=")
		}
	}
	input, err := native.StdinPipe()
	if err != nil {
		return err
	}
	output, err := native.StdoutPipe()
	if err != nil {
		return err
	}
	if err = native.Start(); err != nil {
		return err
	}
	b := &bridge{input: input, routes: map[string]route{}, done: make(chan struct{}), resumePermissions: permissions}
	b.controller = New(b)
	go func() {
		select {
		case <-b.done:
			cancel()
		case <-childCtx.Done():
		}
	}()
	server := &http.Server{Handler: http.HandlerFunc(b.serveControl), ReadHeaderTimeout: 5 * time.Second}
	defer closeWithError(&runErr, "closing native control server", server)
	go func() {
		if e := server.Serve(cl); e != nil && !errors.Is(e, http.ErrServerClosed) {
			b.fail(e)
		}
	}()
	rpcServer := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		c, e := upgrader.Upgrade(w, r, nil)
		if e != nil {
			b.fail(e)
			return
		}
		b.serveClient(c, output)
	})}
	defer closeWithError(&runErr, "closing native RPC server", rpcServer)
	go func() {
		if e := rpcServer.Serve(rl); e != nil && !errors.Is(e, http.ErrServerClosed) {
			b.fail(e)
		}
	}()
	tui := exec.CommandContext(childCtx, "codex", append([]string{"--remote", "unix://" + rpc}, args...)...)
	tui.Stdin = stdin
	tui.Stdout = stdout
	tui.Stderr = stderr
	tuiErr := tui.Run()
	_ = input.Close()
	cancel()
	nativeErr := native.Wait()
	if tuiErr != nil {
		b.mu.Lock()
		failure := b.failure
		b.mu.Unlock()
		return errors.Join(fmt.Errorf("native Codex TUI: %w", tuiErr), failure)
	}
	if nativeErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// closeWithError retains cleanup failures without treating an already-closed
// listener (also owned by its HTTP server) as a second failure.
func closeWithError(result *error, label string, closer io.Closer) {
	if err := closer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		*result = errors.Join(*result, fmt.Errorf("%s: %w", label, err))
	}
}

// resumePermissions carries explicit CLI permissions over the native API.
// Codex's remote TUI rejects these flags on resume before issuing any request.
type resumePermissions struct{ approval, sandbox string }

func prepareResumePermissions(args []string) ([]string, resumePermissions, error) {
	takesValue := func(key string) bool {
		switch key {
		case "--model", "-m", "--config", "-c", "--cd", "-C", "--image", "-i", "--profile", "-p", "--sandbox", "-s", "--ask-for-approval", "-a", "--add-dir", "--local-provider":
			return true
		}
		return false
	}
	resume := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		if args[i] == "resume" {
			resume = true
			break
		}
		if takesValue(args[i]) {
			i++
		}
	}
	if !resume {
		return args, resumePermissions{}, nil
	}
	var p resumePermissions
	var out []string
	bypass := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out = append(out, args[i:]...)
			break
		}
		key, value, inline := strings.Cut(arg, "=")
		start := i
		if takesValue(key) && !inline {
			if i+1 == len(args) {
				return nil, p, fmt.Errorf("native Codex resume: %s requires a value", key)
			}
			i++
			value = args[i]
		}
		switch key {
		case "--dangerously-bypass-approvals-and-sandbox", "--yolo":
			if inline {
				return nil, p, errors.New("native Codex resume: bypass flag does not accept a value")
			}
			bypass = true
		case "--sandbox", "-s":
			if p.sandbox != "" {
				return nil, p, errors.New("native Codex resume: duplicate sandbox flag")
			}
			switch value {
			case "read-only", "workspace-write", "danger-full-access":
				p.sandbox = value
			default:
				return nil, p, fmt.Errorf("native Codex resume: invalid sandbox %q", value)
			}
		case "--ask-for-approval", "-a":
			if p.approval != "" {
				return nil, p, errors.New("native Codex resume: duplicate approval flag")
			}
			switch value {
			case "untrusted", "on-failure", "on-request", "never":
				p.approval = value
			default:
				return nil, p, fmt.Errorf("native Codex resume: invalid approval policy %q", value)
			}
		case "--profile", "-p", "--add-dir", "--approve-for-me", "--not-so-yolo", "--full-auto":
			return nil, p, fmt.Errorf("native Codex resume: %s is not supported by this runtime adapter; use explicit --sandbox and --ask-for-approval", key)
		case "--config", "-c":
			configKey, _, _ := strings.Cut(value, "=")
			root, _, _ := strings.Cut(strings.Trim(strings.TrimSpace(configKey), "\"'"), ".")
			switch root {
			case "approval_policy", "approvals_reviewer", "sandbox_mode", "default_permissions", "permissions", "network", "sandbox_workspace_write":
				return nil, p, fmt.Errorf("native Codex resume: permission config %q requires an explicit supported permission flag", root)
			}
			out = append(out, args[start:i+1]...)
		default:
			out = append(out, args[start:i+1]...)
		}
	}
	// Native Codex gives its bypass flag precedence over explicit -a/-s.
	if bypass {
		p = resumePermissions{approval: "never", sandbox: "danger-full-access"}
	}
	return out, p, nil
}

func (p resumePermissions) apply(raw json.RawMessage) (json.RawMessage, error) {
	if p == (resumePermissions{}) {
		return raw, nil
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	if params == nil {
		return nil, errors.New("native resume parameters missing")
	}
	if p.approval != "" {
		params["approvalPolicy"] = json.RawMessage(strconv.Quote(p.approval))
	}
	if p.sandbox != "" {
		if value := params["permissions"]; len(value) > 0 && string(value) != "null" {
			return nil, errors.New("native resume combines a permission profile and explicit sandbox")
		}
		params["sandbox"] = json.RawMessage(strconv.Quote(p.sandbox))
	}
	return json.Marshal(params)
}

func (p resumePermissions) verify(raw json.RawMessage) error {
	if p == (resumePermissions{}) {
		return nil
	}
	var result struct {
		Approval string `json:"approvalPolicy"`
		Sandbox  struct {
			Type string `json:"type"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	expected := map[string]string{"danger-full-access": "dangerFullAccess", "read-only": "readOnly", "workspace-write": "workspaceWrite"}[p.sandbox]
	if (p.approval != "" && result.Approval != p.approval) || (p.sandbox != "" && result.Sandbox.Type != expected) {
		return errors.New("native Codex resume did not confirm the requested approval and sandbox settings")
	}
	return nil
}
