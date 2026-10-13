package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ---------------------------------------------------------------------------
// Codex approval overlay fixtures
//
// The files under testdata/codex-approval are real `tmux capture-pane -p -S
// -40` captures of stock codex-cli 0.162.1 (installed from npm, no local
// changes) started by gc with permission_mode = "suggest", sitting on approval
// overlays: a command that writes outside the read-only sandbox (exec-*) and an
// apply_patch file creation (edit-*), each at 80x24 and again after resizing
// the window to 160x50. idle-after-deny-* is the same pane after the overlay
// was dismissed.
// ---------------------------------------------------------------------------

func readCodexApprovalFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "codex-approval", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return string(data)
}

func TestParseApprovalPrompt_CodexFixtures(t *testing.T) {
	const execCommand = `printf "%s\n" one two > /tmp/codex-approval-b955b7.txt`
	execLabels := []string{
		"Yes, proceed",
		"Yes, and don't ask again for commands that start with `" + execCommand + "`",
		"No, and tell Codex what to do differently",
	}
	editLabels := []string{
		"Yes, proceed",
		"Yes, and don't ask again for these files",
		"No, and tell Codex what to do differently",
	}
	cases := []struct {
		fixture     string
		tool        string
		input       string
		command     string
		description string
		labels      []string
	}{
		{
			fixture:     "exec-0.162.1-80x24.txt",
			tool:        "exec",
			input:       execCommand,
			command:     execCommand,
			description: "May I write the requested two-line file to /tmp/codex-approval-b955b7.txt? The current environment is read-only.",
			labels:      execLabels,
		},
		{
			fixture:     "exec-0.162.1-160x50.txt",
			tool:        "exec",
			input:       execCommand,
			command:     execCommand,
			description: "May I write the requested two-line file to /tmp/codex-approval-b955b7.txt? The current environment is read-only.",
			labels:      execLabels,
		},
		{
			fixture:     "edit-0.162.1-80x24.txt",
			tool:        "apply_patch",
			input:       "/home/tester/staging/stock-lab/fresh1/approval-edit-b955b7.txt",
			description: "Apply proposed file edits",
			labels:      editLabels,
		},
		{
			fixture:     "edit-0.162.1-160x50.txt",
			tool:        "apply_patch",
			input:       "/home/tester/staging/stock-lab/fresh1/approval-edit-b955b7.txt",
			description: "Apply proposed file edits",
			labels:      editLabels,
		},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			a := parseApprovalPrompt(readCodexApprovalFixture(t, tc.fixture))
			if a == nil {
				t.Fatal("expected the Codex approval overlay to parse, got nil")
			}
			if a.ToolName != tc.tool {
				t.Errorf("ToolName = %q, want %q", a.ToolName, tc.tool)
			}
			if a.Input != tc.input {
				t.Errorf("Input = %q, want %q", a.Input, tc.input)
			}
			if a.Command != tc.command {
				t.Errorf("Command = %q, want %q", a.Command, tc.command)
			}
			if a.Description != tc.description {
				t.Errorf("Description = %q, want %q", a.Description, tc.description)
			}
			if got := approvalOptionLabels(a); strings.Join(got, "\n") != strings.Join(tc.labels, "\n") {
				t.Errorf("option labels = %q, want %q", got, tc.labels)
			}
		})
	}
}

func TestParseApprovalPrompt_CodexRequestIDStableAcrossPaneWidths(t *testing.T) {
	for _, pair := range [][2]string{
		{"exec-0.162.1-80x24.txt", "exec-0.162.1-160x50.txt"},
		{"edit-0.162.1-80x24.txt", "edit-0.162.1-160x50.txt"},
	} {
		narrow := parseApprovalPrompt(readCodexApprovalFixture(t, pair[0]))
		wide := parseApprovalPrompt(readCodexApprovalFixture(t, pair[1]))
		if narrow == nil || wide == nil {
			t.Fatalf("%v: expected both captures to parse, got %v / %v", pair, narrow, wide)
		}
		if approvalHash(narrow) != approvalHash(wide) {
			t.Errorf("%v: request IDs differ across widths: %+v vs %+v", pair, narrow, wide)
		}
	}
}

// The other overlay layouts, as codex-cli 0.162.1 renders them in its own
// snapshot tests (codex-rs/tui/src/bottom_pane/approval_overlay.rs and
// chatwidget/tests/exec_flow.rs).
func TestParseApprovalPrompt_CodexOverlayLayouts(t *testing.T) {
	cases := []struct {
		name    string
		pane    string
		tool    string
		input   string
		command string
		labels  []string
	}{
		{
			name: "network access",
			pane: `
  Do you want to approve network access to "example.com"?

  Reason: network request blocked



› 1. Yes, just this once (y)
  2. Yes, and allow this host for this conversation (a)
  3. Yes, and allow this host in the future (p)
  4. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel`,
			tool:  "exec",
			input: `network access to "example.com"`,
			labels: []string{
				"Yes, just this once",
				"Yes, and allow this host for this conversation",
				"Yes, and allow this host in the future",
				"No, and tell Codex what to do differently",
			},
		},
		{
			name: "permissions",
			pane: `
  Would you like to grant these permissions?

  Reason: need workspace access

  Permission rule: network; read ` + "`/tmp/readme.txt`" + `; write ` + "`/tmp/out.txt`" + `


› 1. Yes, grant these permissions for this turn (y)
  2. Yes, grant for this turn with strict auto review (r)
  3. Yes, grant these permissions for this session (a)
  4. No, continue without permissions (d)

  Press enter to confirm or esc to cancel`,
			tool:  "request_permissions",
			input: "network; read `/tmp/readme.txt`; write `/tmp/out.txt`",
			labels: []string{
				"Yes, grant these permissions for this turn",
				"Yes, grant for this turn with strict auto review",
				"Yes, grant these permissions for this session",
				"No, continue without permissions",
			},
		},
		{
			name: "multi-line command",
			pane: `

  Would you like to run the following command?

  $ python - <<'PY'
  print('hello')
  PY


› 1. Yes, proceed (y)
  2. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel`,
			tool:    "exec",
			input:   "python - <<'PY'\nprint('hello')\nPY",
			command: "python - <<'PY'\nprint('hello')\nPY",
			labels:  []string{"Yes, proceed", "No, and tell Codex what to do differently"},
		},
		{
			name: "wrapped title and footer in a narrow pane",
			pane: `
  Would you like to run the following
  command?

  [… 43 lines] ⌃g view all
› 1. Yes, proceed (y)
  2. No, and tell Codex what to do
     differently (esc)
  Press enter to confirm or esc to
  cancel`,
			tool:   "exec",
			input:  "Would you like to run the following command?",
			labels: []string{"Yes, proceed", "No, and tell Codex what to do differently"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := parseApprovalPrompt(tc.pane)
			if a == nil {
				t.Fatal("expected the Codex approval overlay to parse, got nil")
			}
			if a.ToolName != tc.tool {
				t.Errorf("ToolName = %q, want %q", a.ToolName, tc.tool)
			}
			if a.Input != tc.input {
				t.Errorf("Input = %q, want %q", a.Input, tc.input)
			}
			if a.Command != tc.command {
				t.Errorf("Command = %q, want %q", a.Command, tc.command)
			}
			if got := approvalOptionLabels(a); strings.Join(got, "\n") != strings.Join(tc.labels, "\n") {
				t.Errorf("option labels = %q, want %q", got, tc.labels)
			}
		})
	}
}

func TestParseApprovalPrompt_CodexOverlayMustBeLive(t *testing.T) {
	if a := parseApprovalPrompt(readCodexApprovalFixture(t, "idle-after-deny-0.162.1-80x24.txt")); a != nil {
		t.Fatalf("idle Codex pane parsed as an approval: %+v", a)
	}
	// An answered overlay that scrolled above later history is not live.
	pane := readCodexApprovalFixture(t, "exec-0.162.1-80x24.txt") + `
• Ran printf "%s\n" one two > /tmp/codex-approval-b955b7.txt

› Ask Codex to do anything

  GPT-6-Luna medium · ~/staging/stock-lab/fresh1
  ? for shortcuts`
	if a := parseApprovalPrompt(pane); a != nil {
		t.Fatalf("scrolled-away overlay parsed as live: %+v", a)
	}
	// A title from history does not pair with a later numbered list that is
	// not an approval menu.
	pane = `  Would you like to run the following command?

  $ ls
• Ran ls
  └ notes.txt

› Pick one:
  1. Alpha
› 2. Beta`
	if a := parseApprovalPrompt(pane); a != nil {
		t.Fatalf("stale title paired with an unrelated list: %+v", a)
	}
}

func TestApprovalOptionKey_CodexChoosesByLabel(t *testing.T) {
	network := `
  Do you want to approve network access to "example.com"?

  Reason: network request blocked

› 1. Yes, just this once (y)
  2. Yes, and allow this host for this conversation (a)
  3. Yes, and allow this host in the future (p)
  4. No, and tell Codex what to do differently (esc)

  Press enter to confirm or esc to cancel`
	cases := []struct {
		name   string
		pane   string
		action string
		want   string
	}{
		{"exec approve", readCodexApprovalFixture(t, "exec-0.162.1-80x24.txt"), "approve", "1"},
		{"exec approve_always", readCodexApprovalFixture(t, "exec-0.162.1-80x24.txt"), "approve_always", "2"},
		{"exec deny", readCodexApprovalFixture(t, "exec-0.162.1-80x24.txt"), "deny", "3"},
		{"edit approve", readCodexApprovalFixture(t, "edit-0.162.1-80x24.txt"), "approve", "1"},
		{"edit approve_always", readCodexApprovalFixture(t, "edit-0.162.1-80x24.txt"), "approve_always", "2"},
		{"edit deny", readCodexApprovalFixture(t, "edit-0.162.1-80x24.txt"), "deny", "3"},
		{"network approve", network, "approve", "1"},
		{"network deny", network, "deny", "4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := parseApprovalPrompt(tc.pane)
			if a == nil {
				t.Fatal("expected an approval prompt, got nil")
			}
			got, err := approvalOptionKey(a, tc.action)
			if err != nil {
				t.Fatalf("approvalOptionKey(%q): %v", tc.action, err)
			}
			if got != tc.want {
				t.Fatalf("approvalOptionKey(%q) = %q, want %q (options %q)", tc.action, got, tc.want, approvalOptionLabels(a))
			}
		})
	}
}

// TestPending_ReportsCodexApprovalOverlay pins what the API serves for a
// Codex session waiting on approval: before, /pending was empty while the
// agent sat blocked on the overlay.
func TestPending_ReportsCodexApprovalOverlay(t *testing.T) {
	fe := &fakeExecutor{agentPane: true, out: readCodexApprovalFixture(t, "exec-0.162.1-80x24.txt")}
	provider := &Provider{tm: &Tmux{exec: fe}}
	pending, err := provider.Pending("s")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil {
		t.Fatal("Pending = nil while Codex shows an approval overlay")
	}
	if pending.Kind != "approval" || len(pending.Options) != 3 || pending.Options[0] != "Yes, proceed" {
		t.Fatalf("Pending = %+v, want an approval with the three menu labels", pending)
	}
	if got := pending.Metadata["command"]; got != `printf "%s\n" one two > /tmp/codex-approval-b955b7.txt` {
		t.Fatalf("metadata command = %q", got)
	}
}

// TestNudgeNow_RefusesToTypeIntoCodexApprovalOverlay pins the live failure: a
// submit to a Codex session sitting on an approval overlay was reported
// delivered, while its keys canceled the approval and aborted the turn and
// the message itself was lost.
func TestNudgeNow_RefusesToTypeIntoCodexApprovalOverlay(t *testing.T) {
	fe := &fakeExecutor{agentPane: true, out: readCodexApprovalFixture(t, "exec-0.162.1-80x24.txt")}
	provider := &Provider{tm: &Tmux{exec: fe}}
	err := provider.NudgeNow("s", runtime.TextContent("hello"))
	if !errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("NudgeNow error = %v, want runtime.ErrPendingInteraction", err)
	}
	for _, call := range fe.calls {
		if sendsKeys(call) {
			t.Fatalf("NudgeNow sent keys into a Codex approval overlay: %v", call)
		}
	}
}

func TestRespond_ApprovesCodexOverlayByOptionNumber(t *testing.T) {
	fe := &fakeExecutor{
		agentPane: true,
		// pre-verify capture, #{pane_in_mode} probe (not parked), send-keys,
		// then the verify capture showing the overlay gone.
		outs: []string{
			readCodexApprovalFixture(t, "edit-0.162.1-80x24.txt"),
			"0",
			"",
			readCodexApprovalFixture(t, "idle-after-deny-0.162.1-80x24.txt"),
		},
	}
	provider := &Provider{tm: &Tmux{exec: fe}}
	if err := provider.Respond("s", runtime.InteractionResponse{Action: "approve"}); err != nil {
		t.Fatalf("Respond(approve): %v", err)
	}
	var sent []string
	for _, call := range fe.calls {
		if sendsKeys(call) {
			sent = append(sent, call[len(call)-1])
		}
	}
	if len(sent) != 1 || sent[0] != "1" {
		t.Fatalf("send-keys payloads = %q, want exactly [\"1\"] (\"Yes, proceed\")", sent)
	}
}

// Codex's highlighted option starts with "› ", Codex's ready-prompt prefix,
// so an overlay must not read as an idle composer.
func TestSnapshotPaneIdle_CodexApprovalOverlayIsNotIdle(t *testing.T) {
	fe := &fakeExecutor{agentPane: true, out: readCodexApprovalFixture(t, "exec-0.162.1-80x24.txt")}
	tm := &Tmux{exec: fe}
	if idle, _ := tm.snapshotPaneIdleWithPrefix("s", "› "); idle {
		t.Fatal("pane showing a Codex approval overlay reported idle")
	}
	fe.out = readCodexApprovalFixture(t, "idle-after-deny-0.162.1-80x24.txt")
	idle, err := tm.snapshotPaneIdleWithPrefix("s", "› ")
	if err != nil {
		t.Fatalf("snapshotPaneIdleWithPrefix: %v", err)
	}
	if !idle {
		t.Fatal("idle Codex composer reported busy")
	}
}
