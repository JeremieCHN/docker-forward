package forward

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const dialTimeout = 5 * time.Second

// TCPForwarder 监听宿主机端口，把每个连接代理到当前目标（可热更新）。
type TCPForwarder struct {
	listen string
	ln     net.Listener

	target        atomic.Pointer[Target] // nil 表示目标不可用，新连接 fail fast
	closeOnSwitch atomic.Bool
	closed        atomic.Bool
	active        atomic.Int64

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func NewTCPForwarder(listen string) (*TCPForwarder, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	f := &TCPForwarder{
		listen: listen,
		ln:     ln,
		conns:  make(map[net.Conn]struct{}),
	}
	go f.acceptLoop()
	return f, nil
}

// UpdateTarget 原子替换目标，仅影响新连接；closeOnSwitch 开启时目标变化会断开存量连接。
func (f *TCPForwarder) UpdateTarget(t *Target) {
	old := f.target.Swap(t)
	changed := (old == nil) != (t == nil) || (old != nil && t != nil && *old != *t)
	if changed && f.closeOnSwitch.Load() {
		f.closeTracked()
	}
}

func (f *TCPForwarder) SetCloseOnSwitch(v bool) { f.closeOnSwitch.Store(v) }

func (f *TCPForwarder) ActiveConns() int64 { return f.active.Load() }

func (f *TCPForwarder) Close() {
	if f.closed.CompareAndSwap(false, true) {
		f.ln.Close()
		f.closeTracked()
	}
}

func (f *TCPForwarder) acceptLoop() {
	var delay time.Duration
	for {
		c, err := f.ln.Accept()
		if err != nil {
			if f.closed.Load() {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
				}
				if delay > time.Second {
					delay = time.Second
				}
				time.Sleep(delay)
				continue
			}
			slog.Error("accept failed, listener stopped", "listen", f.listen, "err", err)
			return
		}
		delay = 0
		f.track(c)
		f.active.Add(1)
		go f.serve(c)
	}
}

func (f *TCPForwarder) serve(client net.Conn) {
	defer func() {
		f.active.Add(-1)
		f.untrack(client)
		client.Close()
	}()
	t := f.target.Load()
	if t == nil {
		return
	}
	backend, err := (&net.Dialer{Timeout: dialTimeout}).Dial("tcp", t.String())
	if err != nil {
		slog.Debug("dial target failed", "listen", f.listen, "target", t.String(), "err", err)
		return
	}
	f.track(backend)
	defer func() {
		f.untrack(backend)
		backend.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relay(backend, client)
	}()
	go func() {
		defer wg.Done()
		relay(client, backend)
	}()
	wg.Wait()
}

func relay(dst, src net.Conn) {
	io.Copy(dst, src)
	if tc, ok := dst.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
}

func (f *TCPForwarder) track(c net.Conn) {
	f.mu.Lock()
	f.conns[c] = struct{}{}
	f.mu.Unlock()
}

func (f *TCPForwarder) untrack(c net.Conn) {
	f.mu.Lock()
	delete(f.conns, c)
	f.mu.Unlock()
}

func (f *TCPForwarder) closeTracked() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		c.Close()
	}
}
