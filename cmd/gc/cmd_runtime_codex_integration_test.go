//go:build integration

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual CLI entrypoint: its output wrappers must not make
// os/exec replace a native runtime's inherited file descriptor with a pipe.
func TestRuntimeCodexPreservesNativeOutputDescriptor(t *testing.T) {
	for _, fileOutput := range []bool{true, false} {
		name := "capture"
		if fileOutput {
			name = "file"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			script := "#!/bin/sh\nif [ \"$1\" = app-server ]; then exec cat >/dev/null; fi\nif [ \"$EXPECT_NATIVE_FILE\" = 1 ] && ! test -f /dev/stdout; then echo 'native output descriptor replaced by pipe' >&2; exit 42; fi\nprintf NATIVE_OUTPUT_PRESERVED\n"
			if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GC_SESSION_NAME", "runtime-stdio")
			t.Setenv("GC_INSTANCE_TOKEN", dir)
			t.Setenv("EXPECT_NATIVE_FILE", "0")
			var captured, stderr bytes.Buffer
			var stdout io.Writer = &captured
			outputPath := filepath.Join(dir, "stdout")
			if fileOutput {
				t.Setenv("EXPECT_NATIVE_FILE", "1")
				file, err := os.Create(outputPath)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close() //nolint:errcheck // fixture descriptor
				stdout = file
			}
			if code := run([]string{"runtime", "codex", "--"}, stdout, &stderr); code != 0 {
				t.Fatalf("runtime exit=%d: %s", code, stderr.String())
			}
			output := captured.String()
			if fileOutput {
				data, err := os.ReadFile(outputPath)
				if err != nil {
					t.Fatal(err)
				}
				output = string(data)
			}
			if !strings.Contains(output, "NATIVE_OUTPUT_PRESERVED") {
				t.Fatalf("native output missing: %q", output)
			}
		})
	}
}
