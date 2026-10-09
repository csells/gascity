//go:build integration && !windows

package codexcontrol

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeSocketRestartReclaimsCrashedListeners(t *testing.T) {
	dir := t.TempDir()
	rpc, control := filepath.Join(dir, "r"), filepath.Join(dir, "c")
	for _, p := range []string{rpc, control} {
		l, e := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
		if e != nil {
			t.Fatal(e)
		}
		l.SetUnlinkOnClose(false)
		if e = l.Close(); e != nil {
			t.Fatal(e)
		}
	}
	r, c, e := listenNativeSockets(rpc, control)
	if e != nil {
		t.Fatalf("crash residue blocked same-incarnation restart: %v", e)
	}
	defer func() {
		if e := r.Close(); e != nil {
			t.Error(e)
		}
		if e := c.Close(); e != nil {
			t.Error(e)
		}
	}()
	for _, p := range []string{rpc, control} {
		conn, e := net.Dial("unix", p)
		if e != nil {
			t.Fatal(e)
		}
		if e = conn.Close(); e != nil {
			t.Error(e)
		}
	}
}

func TestNativeSocketRestartPreservesLiveOwner(t *testing.T) {
	dir := t.TempDir()
	rpc, control := filepath.Join(dir, "r"), filepath.Join(dir, "c")
	r, c, e := listenNativeSockets(rpc, control)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := r.Close(); e != nil {
			t.Error(e)
		}
		if e := c.Close(); e != nil {
			t.Error(e)
		}
	}()
	before, e := os.Stat(rpc)
	if e != nil {
		t.Fatal(e)
	}
	r2, c2, e := listenNativeSockets(rpc, control)
	if e == nil {
		_ = r2.Close()
		_ = c2.Close()
		t.Fatal("second runtime took live sockets")
	}
	after, e := os.Stat(rpc)
	if e != nil {
		t.Fatal(e)
	}
	if !os.SameFile(before, after) {
		t.Fatal("live socket replaced")
	}
	conn, e := net.Dial("unix", rpc)
	if e != nil {
		t.Fatal(e)
	}
	if e = conn.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestNativeSocketRestartPreservesUnexpectedFile(t *testing.T) {
	dir := t.TempDir()
	rpc, control := filepath.Join(dir, "r"), filepath.Join(dir, "c")
	if e := os.WriteFile(rpc, []byte("keep"), 0o600); e != nil {
		t.Fatal(e)
	}
	r, c, e := listenNativeSockets(rpc, control)
	if e == nil {
		_ = r.Close()
		_ = c.Close()
		t.Fatal("unexpected file replaced")
	}
	b, e := os.ReadFile(rpc)
	if e != nil || string(b) != "keep" {
		t.Fatalf("unexpected file changed: %q %v", b, e)
	}
}
