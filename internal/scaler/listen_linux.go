//go:build linux

package scaler

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// overlapBind reports that listen can bind a port a previous listen still
// holds, so the hand-off may bind the full server before stopping the standby.
const overlapBind = true

// listen binds addr with SO_REUSEPORT, so the full server can bind the port
// while the standby server still holds it and the hand-off between them has
// no moment at which the port is unbound. The kernel spreads new connections
// over both sockets until the standby's is closed. Both listeners must set
// the option, which is why every bind goes through here.
//
// The price is that a second process in the same network namespace and UID
// can bind the port too, where it used to fail with EADDRINUSE. In a Pod the
// namespace holds one process; locally, two controllers on one machine
// would share KEDA's connections between them instead of one refusing.
func listen(ctx context.Context, addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var opt error
			if err := c.Control(func(fd uintptr) {
				opt = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}
			return opt
		},
	}
	return lc.Listen(ctx, "tcp", addr)
}
