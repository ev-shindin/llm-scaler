//go:build !linux

package scaler

import (
	"context"
	"net"
)

// overlapBind reports that a port cannot be bound while a previous listen
// holds it, so the hand-off must stop the standby before binding the full
// server -- a sub-millisecond gap that only matters where the controller
// actually runs, which is Linux.
const overlapBind = false

// listen binds addr with no socket options: off Linux there is no
// SO_REUSEPORT to overlap the two servers' listeners.
func listen(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}
