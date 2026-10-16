package rule

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"
)

type Rule struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Protocol      string    `json:"protocol"` // tcp | udp
	ListenAddr    string    `json:"listenAddr"`
	ListenPort    int       `json:"listenPort"`
	ContainerName string    `json:"containerName"`
	Network       string    `json:"network,omitempty"`
	TargetPort    int       `json:"targetPort"`
	Enabled       bool      `json:"enabled"`
	CloseOnSwitch bool      `json:"closeOnSwitch,omitempty"`
	// UDPIdleTimeoutSec UDP 会话空闲超时（秒），0 或缺省为 60。仅 udp 协议有意义。
	UDPIdleTimeoutSec int       `json:"udpIdleTimeoutSec,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

const DefaultUDPIdleTimeout = 60 * time.Second

func (r Rule) UDPIdleTimeout() time.Duration {
	if r.UDPIdleTimeoutSec <= 0 {
		return DefaultUDPIdleTimeout
	}
	return time.Duration(r.UDPIdleTimeoutSec) * time.Second
}

type RuleStatus struct {
	RuleID        string    `json:"ruleId"`
	State         string    `json:"state"` // running | disabled | error
	ResolvedIP    string    `json:"resolvedIp,omitempty"`
	ContainerID   string    `json:"containerId,omitempty"`
	Network       string    `json:"network,omitempty"`
	ActiveConns   int64     `json:"activeConns"`
	LastError     string    `json:"lastError,omitempty"`
	LastResolveAt time.Time `json:"lastResolveAt,omitempty"`
}

var containerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func (r Rule) normalizedAddr() string {
	if r.ListenAddr == "" {
		return "0.0.0.0"
	}
	return r.ListenAddr
}

func (r Rule) Listen() string {
	return net.JoinHostPort(r.normalizedAddr(), strconv.Itoa(r.ListenPort))
}

func (r Rule) ListenKey() string {
	return fmt.Sprintf("%s|%s|%d", r.Protocol, r.normalizedAddr(), r.ListenPort)
}

func (r Rule) Validate() error {
	if r.Protocol != "tcp" && r.Protocol != "udp" {
		return fmt.Errorf("protocol must be tcp or udp, got %q", r.Protocol)
	}
	if r.ListenPort < 1 || r.ListenPort > 65535 {
		return fmt.Errorf("listenPort %d out of range", r.ListenPort)
	}
	if r.TargetPort < 1 || r.TargetPort > 65535 {
		return fmt.Errorf("targetPort %d out of range", r.TargetPort)
	}
	if r.ListenAddr != "" && net.ParseIP(r.ListenAddr) == nil {
		return fmt.Errorf("listenAddr %q is not a valid IP", r.ListenAddr)
	}
	if !containerNameRE.MatchString(r.ContainerName) {
		return fmt.Errorf("containerName %q is invalid", r.ContainerName)
	}
	if r.UDPIdleTimeoutSec < 0 {
		return fmt.Errorf("udpIdleTimeoutSec must be >= 0")
	}
	return nil
}

// ConflictError 表示规则集冲突（监听端口冲突、ID 重复），API 层映射为 409。
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// NewID 生成规则 ID。
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "r" + hex.EncodeToString(b[:])
}

// ValidateSet 校验整组规则：语法、ID 唯一、启用规则间监听端口不冲突。
// ID 为空的规则会被自动分配 ID。
func ValidateSet(in []Rule) ([]Rule, error) {
	out := make([]Rule, len(in))
	copy(out, in)
	ids := map[string]bool{}
	keys := map[string]string{}
	for i := range out {
		r := &out[i]
		if r.ID == "" {
			r.ID = NewID()
		}
		if ids[r.ID] {
			return nil, &ConflictError{Msg: fmt.Sprintf("duplicate rule id %q", r.ID)}
		}
		ids[r.ID] = true
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("rule %q (id %s): %w", r.Name, r.ID, err)
		}
		if !r.Enabled {
			continue
		}
		k := r.ListenKey()
		if other, ok := keys[k]; ok {
			return nil, &ConflictError{Msg: fmt.Sprintf("rules %q and %q listen on the same %s", other, r.Name, r.Listen())}
		}
		keys[k] = r.Name
	}
	return out, nil
}
