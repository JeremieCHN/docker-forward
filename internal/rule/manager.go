package rule

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"docker-forward/internal/dockerx"
	"docker-forward/internal/forward"
)

const (
	StateRunning  = "running"
	StateDisabled = "disabled"
	StateError    = "error"
)

type entry struct {
	rule     Rule
	fwd      forward.Forwarder
	unwatch  func()
	resolved *forward.Target
	status   RuleStatus
}

func (e *entry) shutdown() {
	if e.unwatch != nil {
		e.unwatch()
	}
	if e.fwd != nil {
		e.fwd.Close()
	}
}

// Manager 负责规则生命周期编排：校验、diff 应用、目标解析与状态维护。
type Manager struct {
	rs      *dockerx.Resolver
	mu      sync.Mutex
	entries map[string]*entry
	rules   []Rule // 最近一次应用的规则集（有序）
}

// RuleWithStatus 规则 + 运行时状态，供 API 返回。
type RuleWithStatus struct {
	Rule   Rule
	Status RuleStatus
}

func NewManager(rs *dockerx.Resolver) *Manager {
	return &Manager{rs: rs, entries: map[string]*entry{}}
}

func newForwarder(r Rule) (forward.Forwarder, error) {
	switch r.Protocol {
	case "tcp":
		return forward.NewTCPForwarder(r.Listen())
	case "udp":
		return forward.NewUDPForwarder(r.Listen(), r.UDPIdleTimeout())
	default:
		return nil, fmt.Errorf("unsupported protocol %q", r.Protocol)
	}
}

// Apply 全量应用一组规则。校验失败时整体拒绝、保持现状。
func (m *Manager) Apply(rules []Rule) error {
	rules, err := ValidateSet(rules)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]*entry, len(rules))
	for _, r := range rules {
		next[r.ID] = m.reconcile(m.entries[r.ID], r)
	}
	for id, e := range m.entries {
		if _, ok := next[id]; !ok {
			e.shutdown()
			slog.Info("rule removed", "id", id, "name", e.rule.Name)
		}
	}
	m.entries = next
	m.rules = rules
	return nil
}

func (m *Manager) reconcile(old *entry, r Rule) *entry {
	e := old
	if e == nil {
		e = &entry{}
	}
	// 容器名变了或规则停用：退订旧关注
	if e.unwatch != nil && (e.rule.ContainerName != r.ContainerName || !r.Enabled) {
		e.unwatch()
		e.unwatch = nil
	}
	// 监听参数变了或规则停用：关旧 listener
	if e.fwd != nil && (e.rule.ListenKey() != r.ListenKey() || !r.Enabled) {
		e.fwd.Close()
		e.fwd = nil
		e.resolved = nil
	}
	e.rule = r

	if !r.Enabled {
		e.status = RuleStatus{RuleID: r.ID, State: StateDisabled}
		return e
	}
	if e.fwd == nil {
		f, err := newForwarder(r)
		if err != nil {
			e.status = RuleStatus{RuleID: r.ID, State: StateError, LastError: "listen " + r.Listen() + ": " + err.Error()}
			slog.Error("rule listen failed", "id", r.ID, "name", r.Name, "listen", r.Listen(), "err", err)
			return e
		}
		e.fwd = f
		e.resolved = nil
		slog.Info("rule listening", "id", r.ID, "name", r.Name, "proto", r.Protocol, "listen", r.Listen())
	}
	e.fwd.SetCloseOnSwitch(r.CloseOnSwitch)
	if uf, ok := e.fwd.(*forward.UDPForwarder); ok {
		uf.SetIdleTimeout(r.UDPIdleTimeout())
	}
	if e.unwatch == nil {
		id := r.ID
		e.unwatch = m.rs.Watch(r.ContainerName, func() { m.resolve(id) })
	}
	e.status = RuleStatus{RuleID: r.ID, State: StateRunning} // 目标解析中，resolve 会覆盖为最终状态
	go m.resolve(r.ID)
	return e
}

// resolve 重新解析规则目标的容器 IP，变化时热更新数据面。可被事件回调或应用流程触发。
func (m *Manager) resolve(id string) {
	m.mu.Lock()
	e := m.entries[id]
	if e == nil {
		m.mu.Unlock()
		return
	}
	r := e.rule
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	ip, network, cid, err := m.rs.ResolveIP(ctx, r.ContainerName, r.Network)
	cancel()

	m.mu.Lock()
	defer m.mu.Unlock()
	e = m.entries[id]
	if e == nil || e.rule.ContainerName != r.ContainerName || e.rule.TargetPort != r.TargetPort {
		return // 规则在解析期间被改动，结果作废
	}
	now := time.Now()
	if err != nil {
		if e.resolved != nil && e.fwd != nil {
			e.fwd.UpdateTarget(nil)
			e.resolved = nil
		}
		e.status = RuleStatus{RuleID: id, State: StateError, LastError: err.Error(), LastResolveAt: now}
		slog.Warn("resolve failed", "id", id, "container", r.ContainerName, "err", err)
		return
	}
	t := &forward.Target{IP: ip, Port: r.TargetPort}
	if (e.resolved == nil || *e.resolved != *t) && e.fwd != nil {
		from := ""
		if e.resolved != nil {
			from = e.resolved.String()
		}
		e.fwd.UpdateTarget(t)
		slog.Info("target updated", "id", id, "name", r.Name, "container", r.ContainerName, "from", from, "to", t.String(), "network", network)
	}
	e.resolved = t
	e.status = RuleStatus{
		RuleID:        id,
		State:         StateRunning,
		ResolvedIP:    ip,
		Network:       network,
		ContainerID:   cid,
		LastResolveAt: now,
	}
}

// Rules 返回当前规则集（有序副本）。
func (m *Manager) Rules() []Rule {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Rule(nil), m.rules...)
}

// ListWithStatus 返回规则及运行时状态，按规则集顺序。
func (m *Manager) ListWithStatus() []RuleWithStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RuleWithStatus, 0, len(m.rules))
	for _, r := range m.rules {
		rws := RuleWithStatus{Rule: r}
		if e := m.entries[r.ID]; e != nil {
			st := e.status
			if e.fwd != nil {
				st.ActiveConns = e.fwd.ActiveConns()
			}
			rws.Status = st
		}
		out = append(out, rws)
	}
	return out
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		e.shutdown()
	}
	m.entries = map[string]*entry{}
}
