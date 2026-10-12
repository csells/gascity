package runtime

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Screens captured from stock codex-cli 0.162.1 under a managed tmux session.
// Codex draws the composer first; the status line under it ("<model> · <dir>")
// appears only once startup has finished, after any startup dialog.
const (
	codex0162EarlyComposer = `  >_ OpenAI Codex (v0.162.1)
     ~/city

› Ask Codex to do anything
  ? for shortcuts`
	codex0162SettledComposer = `  >_ OpenAI Codex (v0.162.1)
     ~/city
  permissions: YOLO mode

› Ask Codex to do anything
  GPT-6.1-Sol default · ~/city
  ? for shortcuts`
	codex0162HookReview = `  Hooks need review
  4 hooks are new or changed.
  Hooks can run outside the sandbox after you trust them.
› 1. Review hooks
  2. Trust all and continue
  3. Continue without trusting (hooks won't run)
  enter confirm · esc skip`
	claudeComposer = `╭───────────────────────────────────────────╮
│ ✻ Welcome to Claude Code!                 │
╰───────────────────────────────────────────╯

────────────────────────────────────────────
❯
────────────────────────────────────────────
  ⏵⏵ bypass permissions on (shift+tab to cycle)`
)

// fakeLateCodexPane models stock Codex 0.156+ startup as measured live on
// 0.162.1: the composer is drawn first (~0.5s) and "Hooks need review" replaces
// it 1.6-3.0s later. Down moves the menu cursor; Enter confirms its row.
type fakeLateCodexPane struct {
	mu        sync.Mutex
	screen    string
	cursor    int // 0 "Review hooks", 1 "Trust all and continue"
	sent      []string
	confirmed string
}

func (p *fakeLateCodexPane) peek(int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.screen, nil
}

func (p *fakeLateCodexPane) show(screen string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.screen = screen
}

func (p *fakeLateCodexPane) sendKeys(keys ...string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, keys...)
	for _, k := range keys {
		if p.screen != codex0162HookReview {
			continue
		}
		switch k {
		case "Down":
			p.cursor++
		case "Enter":
			if p.cursor == 1 {
				p.confirmed = "trust"
			} else {
				p.confirmed = "review"
			}
			p.screen = codex0162SettledComposer
		}
	}
	return nil
}

// TestAcceptStartupDialogsAnswersCodexHookReviewDrawnAfterComposer pins #6464.
// Codex draws its composer before "Hooks need review", and the composer's "› "
// prompt is the same signal the handlers read as "no dialog is coming". A
// managed session must not park at a dialog that arrives after that prompt.
func TestAcceptStartupDialogsAnswersCodexHookReviewDrawnAfterComposer(t *testing.T) {
	clock := useSteppedDialogClock(t)
	pane := &fakeLateCodexPane{}
	clock.after(500*time.Millisecond, func() { pane.show(codex0162EarlyComposer) })
	clock.after(2200*time.Millisecond, func() { pane.show(codex0162HookReview) })

	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if pane.confirmed != "trust" || !reflect.DeepEqual(pane.sent, []string{"Down", "Enter"}) {
		t.Fatalf("hook review drawn after the composer: sent=%v confirmed=%q, want [Down Enter] trust", pane.sent, pane.confirmed)
	}
}

// TestAcceptStartupDialogsReturnsPromptlyForSettledCodex: a Codex pane that has
// finished startup (status line drawn) costs no extra wait.
func TestAcceptStartupDialogsReturnsPromptlyForSettledCodex(t *testing.T) {
	clock := useSteppedDialogClock(t)
	pane := &fakeLateCodexPane{screen: codex0162SettledComposer}
	start := clock.Now()
	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > time.Second {
		t.Fatalf("settled Codex composer took %v of virtual time", elapsed)
	}
	if len(pane.sent) != 0 {
		t.Fatalf("sent %v with no dialog on screen", pane.sent)
	}
}

// TestAcceptStartupDialogsBoundsCodexStartupWatch: a Codex pane that never
// shows its status line is watched no longer than the dialog timeout.
func TestAcceptStartupDialogsBoundsCodexStartupWatch(t *testing.T) {
	clock := useSteppedDialogClock(t)
	pane := &fakeLateCodexPane{screen: codex0162EarlyComposer}
	start := clock.Now()
	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > 2*dialogPollTimeout {
		t.Fatalf("unsettled Codex composer was watched for %v", elapsed)
	}
	if len(pane.sent) != 0 {
		t.Fatalf("sent %v with no dialog on screen", pane.sent)
	}
}

// TestAcceptStartupDialogsDoesNotDelayNonCodexPrompt: only a Codex pane waits
// for late dialogs; a Claude composer returns as soon as it is ready.
func TestAcceptStartupDialogsDoesNotDelayNonCodexPrompt(t *testing.T) {
	clock := useSteppedDialogClock(t)
	pane := &fakeLateCodexPane{screen: claudeComposer}
	start := clock.Now()
	if err := AcceptStartupDialogsWithTimeout(context.Background(), dialogPollTimeout, pane.peek, pane.sendKeys); err != nil {
		t.Fatalf("AcceptStartupDialogsWithTimeout: %v", err)
	}
	if elapsed := clock.Now().Sub(start); elapsed > time.Second {
		t.Fatalf("Claude composer took %v of virtual time; the late-dialog watch must be Codex-only", elapsed)
	}
}
