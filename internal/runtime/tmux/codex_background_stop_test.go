package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Pane captures from codex-cli 0.162.1 in the GC tmux runtime, taken live: a
// Codex session asked to run `sh -c "sleep 90; touch /tmp/marker"` in the
// foreground, then stopped through POST /v0/city/{city}/session/{id}/stop.
// Codex ran the command in a background terminal, so the interrupt ended the
// turn while the command kept running and later wrote the marker.
var (
	// The turn's status row before the stop. The rows around it are this
	// test's; the status row is verbatim.
	codexWorkingWithBackgroundTerminalPane = []string{
		"› Run sh -c \"sleep 90; touch /tmp/marker\" in the foreground",
		"",
		"• Working (11s • esc to interrupt) · 1 background terminal running · /ps to vie…",
		"› Ask Codex to do anything",
		"  GPT-6-Luna medium · ~/staging/stock-lab/fresh1 · Run foreground shell command",
	}
	// The bottom of the pane right after the stop returned, verbatim.
	codexInterruptedWithBackgroundTerminalPane = []string{
		"■ Conversation interrupted - use /feedback if something went wrong",
		"  1 background terminal running · /ps to view · /stop to close",
		"› Ask Codex to do anything",
		"  GPT-6-Luna medium · ~/staging/stock-lab/fresh1 · Run foreground shell command",
		"  ? for shortcuts                                      ⚠ 1 warning · f2 to view",
	}
)

// fakeCodexPane is a tmux executor that plays an interrupted codex-cli 0.162.1
// pane. It renders the live screens above: the Working status row while the
// turn is still busy, then the interrupted pane, with the background-terminal
// row while a terminal runs. Typing /stop and pressing Enter in the composer
// stops the background terminals the way Codex does, after
// stopAfterCaptures more captures (never, with ignoreStop). Its CODEX_HOME
// holds a rollout whose tail already records the turn as aborted, so the
// interrupt boundary is reached on the first look.
type fakeCodexPane struct {
	mu sync.Mutex

	provider  string
	codexHome string

	// busyCaptures is how many captures still show the Working row.
	busyCaptures int
	// background is how many background terminals are running.
	background int
	// draft is the text in the composer; empty shows the dim placeholder.
	draft string
	// stopAfterCaptures is how many captures after /stop is submitted still
	// report the background terminals.
	stopAfterCaptures int
	ignoreStop        bool

	stopSubmitted bool
	stopCaptures  int
	captures      int
	// events is every literal text ("type:<text>") and key ("key:<key>")
	// that reached the pane, and every pause the runtime took
	// ("sleep:<duration>"), in order.
	events []string
}

func newFakeCodexPane(t *testing.T) *fakeCodexPane {
	t.Helper()
	home := t.TempDir()
	rollout := filepath.Join(home, "sessions", "2026", "10", "12", "rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	aborted := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<turn_aborted>\nThe user interrupted the previous turn on purpose.\n</turn_aborted>"}]}}` + "\n"
	if err := os.WriteFile(rollout, []byte(aborted), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return &fakeCodexPane{provider: "codex", codexHome: home, background: 1}
}

func (f *fakeCodexPane) render(escapes bool) []string {
	if f.captures <= f.busyCaptures {
		return append([]string(nil), codexWorkingWithBackgroundTerminalPane...)
	}
	lines := []string{codexInterruptedWithBackgroundTerminalPane[0]}
	if f.stopSubmitted {
		lines = append(lines, "", "• Stopping all background terminals.", "")
	}
	if f.background > 0 {
		lines = append(lines, codexInterruptedWithBackgroundTerminalPane[1])
	}
	composer := "› " + f.draft
	if f.draft == "" {
		placeholder := "Ask Codex to do anything"
		if escapes {
			placeholder = "\x1b[2m" + placeholder + "\x1b[22m"
		}
		composer = "› " + placeholder
	}
	lines = append(lines, composer)
	return append(lines, codexInterruptedWithBackgroundTerminalPane[3:]...)
}

func (f *fakeCodexPane) execute(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "show-environment":
		switch {
		case slices.Contains(args, "GC_PROVIDER"):
			return "GC_PROVIDER=" + f.provider, nil
		case slices.Contains(args, "CODEX_HOME"):
			return "CODEX_HOME=" + f.codexHome, nil
		}
		return "", errors.New("unknown variable")
	case "display-message":
		if slices.Contains(args, "#{pane_in_mode}") {
			return "0", nil
		}
		return "", nil
	case "capture-pane":
		f.captures++
		if f.stopSubmitted && !f.ignoreStop && f.background > 0 {
			f.stopCaptures++
			if f.stopCaptures > f.stopAfterCaptures {
				f.background = 0
			}
		}
		return strings.Join(f.render(slices.Contains(args, "-e")), "\n"), nil
	case "send-keys":
		f.sendKeys(args)
		return "", nil
	}
	return "", nil
}

func (f *fakeCodexPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *fakeCodexPane) sendKeys(args []string) {
	i := slices.Index(args, "send-keys")
	literal := false
	var keys []string
	for i++; i < len(args); i++ {
		switch args[i] {
		case "-t":
			i++
		case "-l":
			literal = true
		case "-X":
			return // copy-mode command, not input
		default:
			keys = append(keys, args[i])
		}
	}
	if literal {
		text := strings.Join(keys, " ")
		f.events = append(f.events, "type:"+text)
		f.draft += text
		return
	}
	for _, key := range keys {
		f.events = append(f.events, "key:"+key)
		if key == "Enter" {
			if f.draft == "/stop" {
				f.stopSubmitted = true
			}
			f.draft = ""
		}
	}
}

// sleep records a pause the runtime took, without taking it.
func (f *fakeCodexPane) sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "sleep:"+d.String())
	return nil
}

// allEvents returns every event, pauses included.
func (f *fakeCodexPane) allEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// typedOrPressed returns what was typed and pressed, without the pauses.
func (f *fakeCodexPane) typedOrPressed() []string {
	var got []string
	for _, event := range f.allEvents() {
		if !strings.HasPrefix(event, "sleep:") {
			got = append(got, event)
		}
	}
	return got
}

// pausedAfter returns the total of the pauses taken after the first event
// equal to marker.
func (f *fakeCodexPane) pausedAfter(marker string) time.Duration {
	var total time.Duration
	seen := false
	for _, event := range f.allEvents() {
		if event == marker {
			seen = true
			continue
		}
		if d, ok := strings.CutPrefix(event, "sleep:"); ok && seen {
			parsed, err := time.ParseDuration(d)
			if err == nil {
				total += parsed
			}
		}
	}
	return total
}

// newCodexStopTmux returns a Tmux driving pane, whose pauses pane records
// instead of taking.
func newCodexStopTmux(pane *fakeCodexPane) *Tmux {
	tm := NewTmux()
	tm.exec = pane
	tm.codexStopSleep = pane.sleep
	return tm
}

// TestCodexStopBackgroundWorkStopsBackgroundTerminals is the live defect: the
// stop ended the Codex turn, but the command it was running kept going in a
// background terminal. StopBackgroundWork types Codex's own /stop into the
// idle composer, submits it with a plain Enter, and waits until the pane no
// longer reports running background terminals.
func TestCodexStopBackgroundWorkStopsBackgroundTerminals(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.stopAfterCaptures = 2
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-a"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}

	if got, want := pane.typedOrPressed(), []string{"type:/stop", "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the interrupted Codex pane = %q, want %q: the interrupt left its background terminal running", got, want)
	}
	// Codex takes an Enter inside a fast burst of keys for a newline, so the
	// Enter follows a settle pause, and is plain: no Escape, which would close
	// the /stop popup.
	events := pane.allEvents()
	typed := slices.Index(events, "type:/stop")
	if got, want := events[typed:typed+3], []string{"type:/stop", "sleep:" + codexSlashCommandSettle.String(), "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("submit of /stop = %q, want %q", got, want)
	}
	if pane.background != 0 {
		t.Fatalf("background terminals = %d after the wait returned, want 0", pane.background)
	}
	// The wait ends on the first capture that no longer reports them.
	if got, want := pane.stopCaptures, pane.stopAfterCaptures+1; got != want {
		t.Fatalf("captures after /stop = %d, want %d (the wait must end as the row clears)", got, want)
	}
}

// TestCodexStopBackgroundWorkSendsNothingWithoutBackgroundTerminals: an
// interrupted turn that left no background terminal running gets no keys.
func TestCodexStopBackgroundWorkSendsNothingWithoutBackgroundTerminals(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.background = 0
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-b"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got := pane.typedOrPressed(); len(got) != 0 {
		t.Fatalf("keys reaching a Codex pane with no background terminal = %q, want none", got)
	}
}

// TestCodexStopBackgroundWorkLeavesADraftAlone: /stop typed behind text in the
// composer would submit that text with /stop glued to it, so a composer that
// is not empty gets no keys and the background terminal keeps running.
func TestCodexStopBackgroundWorkLeavesADraftAlone(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.draft = "a note someone is typing"
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-c"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got := pane.typedOrPressed(); len(got) != 0 {
		t.Fatalf("keys reaching a Codex composer holding a draft = %q, want none", got)
	}
	if pane.draft != "a note someone is typing" {
		t.Fatalf("composer = %q, want the draft untouched", pane.draft)
	}
}

// TestCodexStopBackgroundWorkWaitsOutTheWorkingRow: the Working row also names
// the background terminal, but a busy composer is no place for /stop. It is
// typed only once the pane reads idle, and only once.
func TestCodexStopBackgroundWorkWaitsOutTheWorkingRow(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.busyCaptures = 2
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-d"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.typedOrPressed(), []string{"type:/stop", "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Codex pane = %q, want %q", got, want)
	}
	if pane.background != 0 {
		t.Fatalf("background terminals = %d, want 0", pane.background)
	}
}

// TestCodexStopBackgroundWorkReportsBackgroundTerminalsThatKeepRunning: when
// the row does not clear, /stop is sent once, the wait is bounded, and the
// stop still succeeds (the turn is over; the runtime logs what is left).
func TestCodexStopBackgroundWorkReportsBackgroundTerminalsThatKeepRunning(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.ignoreStop = true
	tm := newCodexStopTmux(pane)

	if err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-e"); err != nil {
		t.Fatalf("StopBackgroundWork: %v", err)
	}
	if got, want := pane.typedOrPressed(), []string{"type:/stop", "key:Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Codex pane = %q, want %q (one /stop, no retries)", got, want)
	}
	if pane.background != 1 {
		t.Fatalf("background terminals = %d, want the fake's 1 still running", pane.background)
	}
	if got := pane.pausedAfter("key:Enter"); got != codexBackgroundStopWait {
		t.Fatalf("waited %v for the background terminals to stop, want the %v bound", got, codexBackgroundStopWait)
	}
}

// TestCodexPaneReportsBackgroundTerminals pins where the report is read: in
// Codex's status area around the live composer, not in the transcript.
func TestCodexPaneReportsBackgroundTerminals(t *testing.T) {
	afterStop := []string{
		codexInterruptedWithBackgroundTerminalPane[0],
		"",
		"• Stopping all background terminals.",
		"",
		"› Ask Codex to do anything",
		"  GPT-6-Luna medium · ~/staging/stock-lab/fresh1 · Run foreground shell command",
	}
	for _, tc := range []struct {
		name              string
		lines             []string
		running, readable bool
	}{
		{"live interrupted pane", codexInterruptedWithBackgroundTerminalPane, true, true},
		{"live Working row", codexWorkingWithBackgroundTerminalPane, true, true},
		{"after /stop", afterStop, false, true},
		{"plural", []string{"  2 background terminals running · /ps to view · /stop to close", "› ", "  gpt-5.5 low · /tmp/probe"}, true, true},
		{"transcript above the status area", append([]string{
			"• The footer said 1 background terminal running earlier.",
			"",
			"• Done.",
			"",
		}, afterStop...), false, true},
		{"an earlier message, not the live composer", []string{
			"› 1 background terminal running",
			"",
			"• Noted.",
			"",
			"• Stopping all background terminals.",
			"› Ask Codex to do anything",
		}, false, true},
		{"no composer on screen", codexInterruptedWithBackgroundTerminalPane[:2], false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running, readable := codexPaneReportsBackgroundTerminals(tc.lines)
			if running != tc.running || readable != tc.readable {
				t.Fatalf("codexPaneReportsBackgroundTerminals = (%t, %t), want (%t, %t)", running, readable, tc.running, tc.readable)
			}
		})
	}
}

// TestNonCodexStopBackgroundWorkIsUnsupported: a Claude session has no
// background work for the runtime to stop; no keys reach its pane.
func TestNonCodexStopBackgroundWorkIsUnsupported(t *testing.T) {
	pane := newFakeCodexPane(t)
	pane.provider = "claude"
	tm := newCodexStopTmux(pane)

	err := tm.StopBackgroundWork(context.Background(), "codex-bg-stop-f")
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("StopBackgroundWork(claude) = %v, want ErrInteractionUnsupported", err)
	}
	if got := pane.typedOrPressed(); len(got) != 0 {
		t.Fatalf("keys reaching a Claude pane = %q, want none", got)
	}
}
