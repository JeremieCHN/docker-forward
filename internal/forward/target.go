package forward

import (
	"net"
	"strconv"
)

// Target 是转发目标。Forwarder 内部以 atomic.Pointer 持有，可热更新。
type Target struct {
	IP   string
	Port int
}

func (t Target) String() string {
	return net.JoinHostPort(t.IP, strconv.Itoa(t.Port))
}
