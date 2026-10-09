package main

import (
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/gastownhall/gascity/internal/runtime/codexcontrol"
	"github.com/spf13/cobra"
)

// newRuntimeCodexCmd starts an opt-in Codex runtime with native command ownership.
func newRuntimeCodexCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{Use: "codex -- [codex arguments]", Short: "Run Codex with GC-owned command cancellation", DisableFlagParsing: true, RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
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
