//go:build !linux && !darwin

package control

import (
	"errors"
	"net"
)

// peerUID is not available here, so only signed writes are accepted.
func peerUID(net.Conn) (int, error) {
	return -1, errors.New("peer credentials are not supported on this platform")
}
