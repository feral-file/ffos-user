package offlinecache

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func nativeCachePeerError(connection net.Conn) error {
	conn, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.New("native cache requires a Unix connection")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil || int64(credentials.Uid) != int64(os.Geteuid()) {
			peerErr = errors.New("native cache peer UID mismatch")
		}
	})
	if err != nil {
		return err
	}
	return peerErr
}

func nativeCachePeer(connection net.Conn) bool { return nativeCachePeerError(connection) == nil }
