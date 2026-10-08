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
		return codexcontrol.Run(ctx, args, os.Stdin, stdout, stderr)
	}}
}
