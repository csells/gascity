//go:build !windows

package codexcontrol

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The directory lock serializes only listener creation and stale-socket removal.
// Its kernel lifetime is the open descriptor; no process-status file is created.
func listenNativeSockets(rpc, control string) (r, c net.Listener, listenErr error) {
	dir, err := os.Open(filepath.Dir(rpc))
	if err != nil {
		return nil, nil, err
	}
	defer closeWithError(&listenErr, "closing native socket creation lock", dir)
	if err = syscall.Flock(int(dir.Fd()), syscall.LOCK_EX); err != nil {
		return nil, nil, err
	}
	r, err = listenNativeSocket(rpc)
	if err != nil {
		return nil, nil, err
	}
	c, err = listenNativeSocket(control)
	if err != nil {
		closeWithError(&err, "closing partial native RPC listener", r)
		return nil, nil, err
	}
	return r, c, nil
}

func listenNativeSocket(path string) (net.Listener, error) {
	listener, err := net.Listen("unix", path)
	if err == nil {
		return listener, nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return nil, err
	}
	before, statErr := os.Lstat(path)
	if statErr != nil {
		return nil, fmt.Errorf("inspect native socket: %w", statErr)
	}
	if before.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("native socket path is not a socket: %s", path)
	}
	conn, dialErr := net.DialTimeout("unix", path, time.Second)
	if dialErr == nil {
		closeWithError(&err, "closing native owner probe", conn)
		return nil, err
	}
	// Only a refused connection proves there is no listener. Timeout, permission
	// errors and every other ambiguous result preserve the existing path.
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return nil, fmt.Errorf("native socket owner probe: %w", dialErr)
	}
	after, statErr := os.Lstat(path)
	if statErr != nil {
		return nil, fmt.Errorf("recheck native socket: %w", statErr)
	}
	if !os.SameFile(before, after) {
		return nil, errors.New("native socket changed during owner probe")
	}
	if err = os.Remove(path); err != nil {
		return nil, fmt.Errorf("remove crashed native socket: %w", err)
	}
	return net.Listen("unix", path)
}
