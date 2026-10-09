package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime/codexcontrol"
	"github.com/spf13/cobra"
)

// newRuntimeCodexCmd starts an opt-in Codex runtime with native command ownership.
func newRuntimeCodexCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{Use: "codex -- [codex arguments]", Short: "Run Codex with GC-owned command cancellation", DisableFlagParsing: true, RunE: func(cmd *cobra.Command, args []string) (runErr error) {
		defer func() {
			if runErr != nil {
				if _, err := fmt.Fprintf(stderr, "gc runtime codex: %v\n", runErr); err != nil {
					runErr = errors.Join(runErr, err)
				}
			}
		}()
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if err := prepareRuntimeCodexHooks(args); err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer cancel()
		return codexcontrol.Run(ctx, args, os.Stdin, runtimeCodexOutput(stdout), runtimeCodexOutput(stderr))
	}}
}

// os/exec preserves a terminal descriptor only when given its *os.File.
// Unwrap GC's own forwarding writers when they end at a file; retain other
// writers so injected capture buffers and explicit output routing still work.
func runtimeCodexOutput(original io.Writer) io.Writer {
	writer := original
	for {
		switch output := writer.(type) {
		case *switchableWriter:
			if output == nil {
				return original
			}
			writer = output.target
		case *countingWriter:
			if output == nil {
				return original
			}
			writer = output.target
		case *os.File:
			return output
		default:
			return original
		}
	}
}

// A session overlay can briefly reintroduce unbound GC hook commands. Restore
// the same canonical bytes the reconciler owns before the native server or TUI
// reads them: persistent resumes cannot bypass changed-hook consent. Existing
// custom hooks are preserved; absent files and non-city launches stay untouched.
func prepareRuntimeCodexHooks(args []string) error {
	cityPath := strings.TrimSpace(os.Getenv("GC_CITY_PATH"))
	if cityPath == "" {
		return nil
	}
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolving native Codex working directory: %w", err)
	}
	for i, arg := range args {
		if (arg == "-C" || arg == "--cd") && i+1 < len(args) {
			workDir = args[i+1]
		}
		if strings.HasPrefix(arg, "--cd=") {
			workDir = strings.TrimPrefix(arg, "--cd=")
		}
	}
	hookPath := filepath.Join(workDir, ".codex", "hooks.json")
	data, err := os.ReadFile(hookPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading native Codex hooks: %w", err)
	}
	if !json.Valid(data) {
		return fmt.Errorf("invalid native Codex hooks in %s", hookPath)
	}
	if err := hooks.Install(fsys.OSFS{}, cityPath, workDir, []string{"codex"}); err != nil {
		return fmt.Errorf("preparing native Codex hooks: %w", err)
	}
	return nil
}
