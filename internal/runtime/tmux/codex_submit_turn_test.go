package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// codexTurnPane is a tmux executor that plays a codex-cli 0.162.1 pane and
// its rollout across a submitted turn, on the clock of the synctest bubble it
// runs in:
//
//   - literal text goes into the composer; C-u empties it;
//   - Enter on an idle pane with text in the composer starts a turn: the
//     composer empties and the rollout records task_started (created on the
//     first turn, as Codex materializes its rollout lazily);
//   - the turn records the submitted text as a user message only recordAfter
//     into the turn (Codex runs the UserPromptSubmit hook GC installs first);
//   - its "Working (… esc to interrupt)" row shows only busyHiddenFor into the
//     turn (a contended host draws it late, or never within the window);
//   - Escape on a running turn aborts it ("turn_aborted", reason interrupted):
//     a submitted text not yet recorded is lost, as it was live.
//
// earlierTurnFor > 0 starts the pane inside an earlier turn that shows its
// Working row from the start and ends by itself after that long.
// selfAbortAfter > 0 makes a submitted turn abort by itself that long in, an
// abort GC did not cause.
type codexTurnPane struct {
	mu sync.Mutex

	session   string
	codexHome string
	rollout   string

	busyHiddenFor  time.Duration
	recordAfter    time.Duration
	selfAbortAfter time.Duration
	// swallowEnters is how many Enters the composer takes as a newline instead
	// of a submit (#4706: codex reads a lone Enter inside a paste burst so).
	swallowEnters int

	draft     string
	running   bool
	turnStart time.Time
	turnSeq   int
	timers    []*time.Timer

	// recorded is every text the rollout recorded as a user message.
	recorded []string
	// aborted counts turns an Escape cut short.
	aborted int
	// events is every literal text ("type:<text>") and key ("key:<key>")
	// that reached the pane, in order.
	events []string
}

func newCodexTurnPane(t *testing.T, session string) *codexTurnPane {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "15")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return &codexTurnPane{
		session:       session,
		codexHome:     home,
		rollout:       filepath.Join(dir, "rollout-2026-10-15T08-42-16-01a13eb9-f318-7c32-87de-d469fe118a8b.jsonl"),
		busyHiddenFor: 300 * time.Millisecond,
		recordAfter:   50 * time.Millisecond,
	}
}

// startEarlierTurn puts the pane inside a turn that ends by itself after d.
func (f *codexTurnPane) startEarlierTurn(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	f.turnStart = time.Now()
	f.busyHiddenFor = 0
	f.turnSeq++
	seq := f.turnSeq
	f.timers = append(f.timers, time.AfterFunc(d, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.running && f.turnSeq == seq {
			f.running = false
		}
	}))
}

// stop cancels the pane's pending turn events so the bubble can end.
func (f *codexTurnPane) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, timer := range f.timers {
		timer.Stop()
	}
}

func (f *codexTurnPane) writeRollout(kind string, payload map[string]any) {
	line, _ := json.Marshal(map[string]any{
		"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"type":      kind,
		"payload":   payload,
	})
	file, err := os.OpenFile(f.rollout, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		panic(err)
	}
	defer file.Close() //nolint:errcheck
	if _, err := file.Write(append(line, '\n')); err != nil {
		panic(err)
	}
}

// startTurn begins the turn that carries text. The caller holds mu.
func (f *codexTurnPane) startTurn(text string) {
	if _, err := os.Stat(f.rollout); errors.Is(err, os.ErrNotExist) {
		f.writeRollout("session_meta", map[string]any{"id": "01a13eb9-f318-7c32-87de-d469fe118a8b", "cli_version": "0.162.1", "originator": "codex-tui"})
	}
	f.running = true
	f.turnStart = time.Now()
	f.turnSeq++
	seq := f.turnSeq
	f.writeRollout("event_msg", map[string]any{"type": "task_started", "turn_id": "turn-" + time.Now().Format("150405.000")})
	f.writeRollout("response_item", map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "<environment_context>\n  <cwd>/w</cwd>\n</environment_context>"}}})
	f.timers = append(f.timers, time.AfterFunc(f.recordAfter, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.running || f.turnSeq != seq {
			return
		}
		f.recorded = append(f.recorded, text)
		f.writeRollout("response_item", map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": text}}})
		f.writeRollout("event_msg", map[string]any{"type": "user_message", "message": text})
	}))
	if f.selfAbortAfter > 0 {
		f.timers = append(f.timers, time.AfterFunc(f.selfAbortAfter, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.running && f.turnSeq == seq {
				f.abortTurn()
			}
		}))
	}
}

// abortTurn ends the running turn as Codex does on Escape. The caller holds mu.
func (f *codexTurnPane) abortTurn() {
	f.running = false
	f.turnSeq++
	f.writeRollout("event_msg", map[string]any{"type": "turn_aborted", "reason": "interrupted"})
}

func (f *codexTurnPane) render(escapes bool) []string {
	lines := []string{">_ OpenAI Codex (v0.162.1)", ""}
	if f.running && time.Since(f.turnStart) >= f.busyHiddenFor {
		lines = append(lines, "• Working (1s • esc to interrupt)", "")
	}
	composer := codexPromptPrefix + f.draft
	if f.draft == "" {
		placeholder := "Ask Codex to do anything"
		if escapes {
			placeholder = "\x1b[2m" + placeholder + "\x1b[22m"
		}
		composer = codexPromptPrefix + placeholder
	}
	return append(lines, composer, "", "  gpt-6.1-sol low · ~/w")
}

func (f *codexTurnPane) execute(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "show-environment":
		switch {
		case slices.Contains(args, "GC_PROVIDER"):
			return "GC_PROVIDER=codex", nil
		case slices.Contains(args, "CODEX_HOME"):
			return "CODEX_HOME=" + f.codexHome, nil
		case slices.Contains(args, sessionReadyPromptEnvKey):
			return sessionReadyPromptEnvKey + "=" + codexPromptPrefix, nil
		}
		return "", errors.New("unknown variable")
	case "display-message":
		switch {
		case slices.Contains(args, "#{pane_in_mode}"):
			return "0", nil
		case slices.Contains(args, "#{session_name}|#{session_attached}"):
			return f.session + "|0", nil
		}
		return "", nil
	case "capture-pane":
		return strings.Join(f.render(slices.Contains(args, "-e")), "\n"), nil
	case "send-keys":
		f.sendKeys(args)
	}
	return "", nil
}

func (f *codexTurnPane) executeCtx(_ context.Context, args []string) (string, error) {
	return f.execute(args)
}

func (f *codexTurnPane) sendKeys(args []string) {
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
		switch key {
		case "C-u":
			f.draft = ""
		case "Escape":
			if f.running {
				f.aborted++
				f.abortTurn()
			}
		case "Enter":
			switch {
			case f.swallowEnters > 0:
				f.swallowEnters--
			case !f.running && f.draft != "":
				text := f.draft
				f.draft = ""
				f.startTurn(text)
			}
		}
	}
}

// keysPressed returns the named keys the pane received, in order.
func (f *codexTurnPane) keysPressed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for _, event := range f.events {
		if key, ok := strings.CutPrefix(event, "key:"); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

func (f *codexTurnPane) snapshot() (recorded []string, aborted int, events []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.recorded), f.aborted, slices.Clone(f.events)
}

// runCodexTurnNudge delivers message through NudgeSession to a pane set up by
// setup, inside a synctest bubble so every wait takes virtual time only.
func runCodexTurnNudge(t *testing.T, session string, setup func(*codexTurnPane)) (*codexTurnPane, error) {
	message := codexTurnMarker
	t.Helper()
	// The bubble creates this session's nudge lock channel; drop it so
	// nothing outside the bubble ever touches it.
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	var pane *codexTurnPane
	var err error
	synctest.Test(t, func(t *testing.T) {
		pane = newCodexTurnPane(t, session)
		setup(pane)
		tm := &Tmux{cfg: DefaultConfig(), exec: pane}
		err = tm.NudgeSession(session, message)
		// Let the turn the submit started play out on the bubble's clock (no
		// wall time passes), then cancel what is left of it.
		<-time.After(time.Minute)
		pane.stop()
	})
	return pane, err
}

const codexTurnMarker = "Reply with exactly FIRST_SUBMIT_MARKER. Do not use tools or change files."

// TestCodexSubmitLeavesTheTurnItStartedRunning is the live defect
// (healthy-direct, 2026-10-15): GC's Escape+Enter started the Codex turn
// (turn 01a13eba-340a at 08:42:32.842Z), but the pane showed no busy row
// within the confirm window, so the confirm loop re-sent Escape+Enter. The
// Escape aborted the turn 1609 ms in (turn_aborted, reason interrupted),
// before Codex recorded the message, and the submit still reported success
// because the composer had drained. A drained composer is the evidence the
// submit landed: the Escape-led sequence must not be sent again into it.
func TestCodexSubmitLeavesTheTurnItStartedRunning(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-hidden-busy", func(p *codexTurnPane) {
		p.busyHiddenFor = time.Hour // the Working row never shows in the window
		p.recordAfter = 3 * time.Second
	})

	recorded, aborted, events := pane.snapshot()
	if aborted != 0 {
		t.Fatalf("the submit aborted %d Codex turn(s); events %q", aborted, events)
	}
	if got, want := pane.keysPressed(), []string{"C-u", "Escape", "Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %q, want %q: one Escape+Enter submit, never repeated into the turn it started", got, want)
	}
	if !slices.Equal(recorded, []string{codexTurnMarker}) {
		t.Fatalf("Codex recorded %q, want the submitted message once", recorded)
	}
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil: Codex's rollout recorded the message", err)
	}
}

// TestCodexSubmitAbortedBeforeItWasRecordedIsNotDelivered: when the turn the
// submit started ends before Codex records the message (here an abort GC did
// not cause), the message never landed. A drained composer is not proof of
// delivery then, and the submit must not report success.
func TestCodexSubmitAbortedBeforeItWasRecordedIsNotDelivered(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-lost", func(p *codexTurnPane) {
		p.busyHiddenFor = time.Hour
		p.recordAfter = 3 * time.Second
		p.selfAbortAfter = time.Second
	})

	if recorded, _, _ := pane.snapshot(); len(recorded) != 0 {
		t.Fatalf("Codex recorded %q, want nothing (the fixture aborts first)", recorded)
	}
	if err == nil || errors.Is(err, runtime.ErrNudgeDeliveredUnobserved) {
		t.Fatalf("NudgeSession = %v, want a not-delivered error", err)
	}
	if !errors.Is(err, ErrNudgeSubmitLost) {
		t.Fatalf("NudgeSession = %v, want ErrNudgeSubmitLost", err)
	}
	if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("NudgeSession = %v, want it to wrap ErrNudgeSubmitUnconfirmed so retrying callers retry it", err)
	}
}

// TestCodexSubmitWaitsOutARunningTurn: Codex reads Escape as "interrupt the
// running turn", so a submit that finds a turn running types nothing and
// presses nothing until the turn ends, then submits once.
func TestCodexSubmitWaitsOutARunningTurn(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-after-turn", func(p *codexTurnPane) {
		p.startEarlierTurn(3 * time.Second)
	})
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil", err)
	}
	recorded, aborted, events := pane.snapshot()
	if aborted != 0 {
		t.Fatalf("the submit aborted %d Codex turn(s); events %q", aborted, events)
	}
	if !slices.Equal(recorded, []string{codexTurnMarker}) {
		t.Fatalf("Codex recorded %q, want the submitted message once", recorded)
	}
}

// TestCodexSubmitDefersWhileATurnOutlastsTheReadyWait: a turn still running
// when the wait for an idle composer runs out gets no keys at all, and the
// caller is told to deliver later (runtime.ErrSessionBusy), which the session
// layer turns into a queued submit.
func TestCodexSubmitDefersWhileATurnOutlastsTheReadyWait(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-busy", func(p *codexTurnPane) {
		p.startEarlierTurn(time.Hour)
	})
	if !errors.Is(err, runtime.ErrSessionBusy) {
		t.Fatalf("NudgeSession = %v, want runtime.ErrSessionBusy", err)
	}
	if _, aborted, events := pane.snapshot(); aborted != 0 || len(events) != 0 {
		t.Fatalf("the busy Codex pane received %q (aborted %d), want nothing typed or pressed", events, aborted)
	}
}

// TestCodexSubmitResendsWhileTheDraftWaits keeps the #4706 recovery: when the
// first Enter is taken as a newline, the message still sits in the idle
// composer, and only then is the Escape+Enter submit sent again.
func TestCodexSubmitResendsWhileTheDraftWaits(t *testing.T) {
	pane, err := runCodexTurnNudge(t, "codex-submit-swallowed-enter", func(p *codexTurnPane) {
		p.swallowEnters = 1
	})
	if err != nil {
		t.Fatalf("NudgeSession = %v, want nil", err)
	}
	if got, want := pane.keysPressed(), []string{"C-u", "Escape", "Enter", "Escape", "Enter"}; !slices.Equal(got, want) {
		t.Fatalf("keys = %q, want %q", got, want)
	}
	if recorded, aborted, _ := pane.snapshot(); aborted != 0 || !slices.Equal(recorded, []string{codexTurnMarker}) {
		t.Fatalf("recorded %q, aborted %d; want the message once and no abort", recorded, aborted)
	}
}

// The rollout reading behind the Codex delivery verdict, on the rows the live
// healthy-direct rollout held after GC's submit (no pane frames survive): the
// environment context, task_started, then turn_aborted, and never the marker.
func TestCodexSubmitRecordReadsTheRollout(t *testing.T) {
	since := time.Date(2026, 10, 15, 8, 42, 30, 490_000_000, time.UTC)
	const head = `{"timestamp":"2026-10-15T08:42:32.878Z","type":"session_meta","payload":{"id":"01a13eb9-f318-7c32-87de-d469fe118a8b","cli_version":"0.162.1"}}
{"timestamp":"2026-10-15T08:42:32.879Z","type":"event_msg","payload":{"type":"task_started","turn_id":"01a13eba-340a-7fd3-8533-2aa0a7bd4e68"}}
{"timestamp":"2026-10-15T08:42:32.880Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/home/user/city</cwd>\n</environment_context>"}]}}
`
	const aborted = `{"timestamp":"2026-10-15T08:42:34.479Z","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"01a13eba-340a-7fd3-8533-2aa0a7bd4e68","reason":"interrupted","duration_ms":1609}}
`
	const recorded = `{"timestamp":"2026-10-15T08:42:33.500Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Reply with exactly FIRST_SUBMIT_MARKER.\nDo not use tools or change files."}]}}
`
	const earlierAbort = `{"timestamp":"2026-10-15T08:42:29.000Z","type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}
`
	for _, tt := range []struct {
		name string
		tail string
		want codexSubmitRecord
	}{
		{"live: aborted before the message was recorded", head + aborted, codexSubmitLost},
		{"recorded, then aborted", head + recorded + aborted, codexSubmitLanded},
		{"recorded", head + recorded, codexSubmitLanded},
		{"turn still starting", head, codexSubmitUnknown},
		{"an abort before the submit is not this submit's", earlierAbort + head, codexSubmitUnknown},
		{"torn first line", `ab"}` + "\n" + head + aborted, codexSubmitLost},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := readCodexSubmitRecord(tt.tail, since, codexTurnMarker); got != tt.want {
				t.Fatalf("readCodexSubmitRecord = %v, want %v", got, tt.want)
			}
		})
	}
}

// A confirm loop for an Escape-led submit sends again only when the look
// before the send shows the message still waiting; never on a pane whose
// composer drained, and never when the look fails.
func TestSubmitAndConfirmWithholdsAnInterruptingResend(t *testing.T) {
	for _, tt := range []struct {
		name      string
		mayResend func() (bool, error)
		wantSends int
	}{
		{"draft still waiting", func() (bool, error) { return true, nil }, submitEnterMaxSends},
		{"composer drained", func() (bool, error) { return false, nil }, 1},
		{"look failed", func() (bool, error) { return false, errors.New("capture-pane: timed out") }, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sends := 0
			confirmed, _, sent, err := submitAndConfirm(func() error { sends++; return nil }, func() {},
				func() (bool, error) { return false, nil }, tt.mayResend, noSleep)
			if err != nil || confirmed {
				t.Fatalf("submitAndConfirm = %t, %v; want unconfirmed, nil", confirmed, err)
			}
			if sends != tt.wantSends || sent != tt.wantSends {
				t.Fatalf("sends = %d (reported %d), want %d", sends, sent, tt.wantSends)
			}
		})
	}
}

// The first submit of an Escape-led sequence waits for an idle reading: a
// pane busy at the baseline gets its submit only after it reads idle.
func TestSubmitAndConfirmHoldsTheFirstInterruptingSubmitWhileBusy(t *testing.T) {
	reads, sends := 0, 0
	busy := func() (bool, error) {
		reads++
		if sends > 0 {
			return true, nil // the submit started a turn
		}
		return reads <= 3, nil // an earlier turn ends after three reads
	}
	confirmed, _, sent, err := submitAndConfirm(func() error { sends++; return nil }, func() {}, busy,
		func() (bool, error) { return true, nil }, noSleep)
	if err != nil || !confirmed {
		t.Fatalf("submitAndConfirm = %t, %v; want confirmed", confirmed, err)
	}
	if sent != 1 {
		t.Fatalf("sends = %d, want 1, after the earlier turn ended", sent)
	}
}

// TestCodexStartupNudgeHoldsItsSubmitWhileATurnRuns: the startup nudge takes
// no ready wait, so it can type into a pane already running a turn. Its
// Escape-led submit is then held back for the whole confirm window: no
// Escape reaches the turn, and the error says both "busy" (a deferring caller
// queues the message) and "unconfirmed" (the startup ladder re-runs the
// delivery, whose clear replaces the typed text).
func TestCodexStartupNudgeHoldsItsSubmitWhileATurnRuns(t *testing.T) {
	const session = "codex-startup-busy"
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	synctest.Test(t, func(t *testing.T) {
		pane := newCodexTurnPane(t, session)
		pane.startEarlierTurn(time.Hour)
		tm := &Tmux{cfg: DefaultConfig(), exec: pane}

		err := tm.nudgeStartupSession(session, codexTurnMarker)
		pane.stop()

		if !errors.Is(err, runtime.ErrSessionBusy) || !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
			t.Fatalf("nudgeStartupSession = %v, want runtime.ErrSessionBusy and ErrNudgeSubmitUnconfirmed", err)
		}
		if slices.Contains(pane.keysPressed(), "Escape") || slices.Contains(pane.keysPressed(), "Enter") {
			t.Fatalf("keys = %q, want no submit into the running turn", pane.keysPressed())
		}
		if _, aborted, _ := pane.snapshot(); aborted != 0 {
			t.Fatalf("aborted %d turn(s), want none", aborted)
		}
	})
}
