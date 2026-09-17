//go:build linux

package scaler

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// listen binds addr with SO_REUSEPORT, so the full server can bind the port
// while the standby server still holds it and the hand-off between them has
// no moment at which the port is unbound. The kernel spreads new connections
// over both sockets until the standby's is closed. Both listeners must set
// the option, which is why every bind goes through here.
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
