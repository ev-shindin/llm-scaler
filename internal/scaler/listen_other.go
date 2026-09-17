//go:build !linux

package scaler

import (
	"context"
	"net"
)

// listen binds addr. Off Linux there is no SO_REUSEPORT to overlap the two
// servers' listeners, so the hand-off closes the standby's socket before the
// full server binds -- a sub-millisecond gap that only matters where the
// controller actually runs, which is Linux.
func listen(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}
