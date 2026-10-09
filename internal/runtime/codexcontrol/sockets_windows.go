//go:build windows

package codexcontrol

import "net"

func listenNativeSockets(rpc, control string) (net.Listener, net.Listener, error) {
	r, e := net.Listen("unix", rpc)
	if e != nil {
		return nil, nil, e
	}
	c, e := net.Listen("unix", control)
	if e != nil {
		closeWithError(&e, "closing partial native RPC listener", r)
		return nil, nil, e
	}
	return r, c, nil
}
