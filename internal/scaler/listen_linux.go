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
// Not quite gapless: a connection the kernel has completed and queued on the
// standby's socket, but that Go has not yet accepted when that socket closes,
// is reset (unless net.ipv4.tcp_migrate_req is on, which it is not by
// default). That is the microseconds between the full server binding and
// the standby's listener closing, against the seconds the port used to be
// unbound; it is not zero.
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
