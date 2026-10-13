package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// primeImplicitProviderSession runs the SessionStart hook prime for a session
// of the claude provider's implicit agent (the template every
// `POST /sessions {"kind":"provider","name":"claude"}` session gets) with the
// given GC_SESSION_ORIGIN, and returns what prime writes.
func primeImplicitProviderSession(t *testing.T, origin string) string {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`
[workspace]
name = "gastown"

[providers.claude]
base = "builtin:claude"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "my-chat")
	t.Setenv("GC_TEMPLATE", "claude")
	t.Setenv("GC_SESSION_NAME", "s-gc-chat")
	t.Setenv("GC_SESSION_ID", "gc-chat")
	t.Setenv("GC_SESSION_ORIGIN", origin)

	var stdout, stderr bytes.Buffer
	if code := doPrimeWithMode(nil, &stdout, &stderr, true, false); code != 0 {
		t.Fatalf("doPrimeWithMode() = %d, want 0; stderr=%q", code, stderr.String())
	}
	return stdout.String()
}

// TestDoPrimeWithHook_ManualProviderSessionGetsNoPoolWorkerPersona pins #7662.
// A client's provider chat runs under the provider's implicit agent only
// because that agent is named after the provider. Priming it as a pool worker
// told the agent to claim queue work and drain-ack, and live it did exactly
// that after the client's first message, ending the conversation.
func TestDoPrimeWithHook_ManualProviderSessionGetsNoPoolWorkerPersona(t *testing.T) {
	worker := primeImplicitProviderSession(t, "ephemeral")
	if strings.TrimSpace(worker) == "" {
		t.Fatal("a pool-spawned session of the implicit agent got no prompt; the control case is broken")
	}

	chat := primeImplicitProviderSession(t, "manual")
	for _, line := range strings.Split(worker, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 24 {
			continue
		}
		if strings.Contains(chat, line) {
			t.Fatalf("a client-created provider session was primed with the worker persona (%q)\nprime output: %q", line, chat)
		}
	}
}
