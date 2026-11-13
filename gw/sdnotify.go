package gateway

import (
	"net"
	"os"
	"time"
)

// sd_notify support without a dependency: one datagram to the unix
// socket named by $NOTIFY_SOCKET, which systemd sets for Type=notify
// units. Unset, every call is a no-op.

// sdNotify sends state ("READY=1", "STOPPING=1") to $NOTIFY_SOCKET.
// Returns nil when the variable is unset.
func sdNotify(state string) error {
	return sdNotifyTo(os.Getenv("NOTIFY_SOCKET"), state)
}

func sdNotifyTo(sock, state string) error {
	if sock == "" {
		return nil
	}
	if sock[0] == '@' { // abstract socket
		sock = "\x00" + sock[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}

// notifyWhenReady sends READY=1 once g reports "serving" (nil g: at
// once), then returns. It also returns when stop closes.
func notifyWhenReady(g *Gateway, stop <-chan struct{}) {
	if os.Getenv("NOTIFY_SOCKET") == "" {
		return
	}
	for g != nil {
		if _, code := g.healthState(); code == 200 {
			break
		}
		select {
		case <-stop:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = sdNotify("READY=1")
}
