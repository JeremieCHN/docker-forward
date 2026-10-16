package forward

import (
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const udpBufSize = 64 * 1024

type udpSession struct {
	client   *net.UDPAddr
	backend  *net.UDPConn
	target   Target
	lastSeen atomic.Int64
}

// UDPForwarder 按客户端源地址维护会话表，把每个会话代理到当前目标。
type UDPForwarder struct {
	listen string
	conn   *net.UDPConn

	target        atomic.Pointer[Target]
	closeOnSwitch atomic.Bool
	idleTimeout   atomic.Int64 // 纳秒
	closed        atomic.Bool
	done          chan struct{}

	mu       sync.Mutex
	sessions map[string]*udpSession
}

func NewUDPForwarder(listen string, idleTimeout time.Duration) (*UDPForwarder, error) {
	addr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	f := &UDPForwarder{
		listen:   listen,
		conn:     conn,
		done:     make(chan struct{}),
		sessions: make(map[string]*udpSession),
	}
	f.idleTimeout.Store(int64(idleTimeout))
	go f.readLoop()
	go f.reapLoop()
	return f, nil
}

// UpdateTarget 热更新目标；closeOnSwitch 开启时立即清空会话表，
// 否则旧会话在该客户端下次发包时迁移到新目标（后端方向保持到被替换或空闲回收）。
func (f *UDPForwarder) UpdateTarget(t *Target) {
	old := f.target.Swap(t)
	changed := (old == nil) != (t == nil) || (old != nil && t != nil && *old != *t)
	if changed && f.closeOnSwitch.Load() {
		f.mu.Lock()
		for k, s := range f.sessions {
			delete(f.sessions, k)
			s.backend.Close()
		}
		f.mu.Unlock()
	}
}

func (f *UDPForwarder) SetCloseOnSwitch(v bool) { f.closeOnSwitch.Store(v) }

func (f *UDPForwarder) SetIdleTimeout(d time.Duration) { f.idleTimeout.Store(int64(d)) }

func (f *UDPForwarder) ActiveConns() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.sessions))
}

func (f *UDPForwarder) Close() {
	if f.closed.CompareAndSwap(false, true) {
		close(f.done)
		f.conn.Close()
		f.mu.Lock()
		for k, s := range f.sessions {
			delete(f.sessions, k)
			s.backend.Close()
		}
		f.mu.Unlock()
	}
}

func (f *UDPForwarder) readLoop() {
	buf := make([]byte, udpBufSize)
	for {
		n, clientAddr, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			if f.closed.Load() {
				return
			}
			slog.Error("udp read failed, listener stopped", "listen", f.listen, "err", err)
			return
		}
		t := f.target.Load()
		if t == nil {
			continue // 目标不可用：丢包
		}
		s := f.getOrCreateSession(clientAddr, *t)
		if s == nil {
			continue
		}
		if _, err := s.backend.Write(buf[:n]); err != nil {
			slog.Debug("udp write to backend failed", "listen", f.listen, "target", s.target.String(), "err", err)
			f.removeSession(clientAddr.String(), s)
			s.backend.Close()
			continue
		}
		s.lastSeen.Store(time.Now().UnixNano())
	}
}

func (f *UDPForwarder) getOrCreateSession(clientAddr *net.UDPAddr, t Target) *udpSession {
	key := clientAddr.String()
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[key]; ok {
		if s.target == t {
			return s
		}
		// 目标已切换：旧会话替换
		delete(f.sessions, key)
		s.backend.Close()
	}
	backend, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(t.IP), Port: t.Port})
	if err != nil {
		slog.Debug("udp dial target failed", "listen", f.listen, "target", t.String(), "err", err)
		return nil
	}
	s := &udpSession{client: clientAddr, backend: backend, target: t}
	s.lastSeen.Store(time.Now().UnixNano())
	f.sessions[key] = s
	go f.sessionReadLoop(key, s)
	return s
}

// sessionReadLoop 把后端回包转给客户端。会话被替换/关闭时 Read 报错退出。
func (f *UDPForwarder) sessionReadLoop(key string, s *udpSession) {
	buf := make([]byte, udpBufSize)
	for {
		n, err := s.backend.Read(buf)
		if err != nil {
			f.removeSession(key, s)
			s.backend.Close()
			return
		}
		if _, err := f.conn.WriteToUDP(buf[:n], s.client); err != nil {
			if !f.closed.Load() {
				slog.Debug("udp write to client failed", "listen", f.listen, "client", s.client, "err", err)
			}
			return
		}
		s.lastSeen.Store(time.Now().UnixNano())
	}
}

func (f *UDPForwarder) removeSession(key string, s *udpSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur, ok := f.sessions[key]; ok && cur == s {
		delete(f.sessions, key)
	}
}

func (f *UDPForwarder) reapLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-f.done:
			return
		case now := <-ticker.C:
			idle := time.Duration(f.idleTimeout.Load())
			f.mu.Lock()
			for k, s := range f.sessions {
				if now.Sub(time.Unix(0, s.lastSeen.Load())) > idle {
					delete(f.sessions, k)
					s.backend.Close()
				}
			}
			f.mu.Unlock()
		}
	}
}
