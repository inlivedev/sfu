//go:build unix

package sfu

import (
	"golang.org/x/sys/unix"
	"syscall"
)

func setSocketOptions(network, address string, conn syscall.RawConn) error {
	var operr error
	if err := conn.Control(func(fd uintptr) {
		operr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}

	return operr
}
