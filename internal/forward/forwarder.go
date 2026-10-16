package forward

// Forwarder 是数据面抽象：TCP/UDP 均实现，供规则管理器统一编排。
type Forwarder interface {
	// UpdateTarget 原子替换目标（nil 表示目标不可用），仅影响新连接/新会话。
	UpdateTarget(t *Target)
	SetCloseOnSwitch(v bool)
	// ActiveConns TCP 为活跃连接数，UDP 为活跃会话数。
	ActiveConns() int64
	Close()
}
