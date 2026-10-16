package dockerx

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// Resolver 维护"关注集"（被启用规则引用的容器名）的订阅关系，
// 订阅 Docker 事件流；任何相关事件触发去抖后的全量刷新回调。
type Resolver struct {
	cli          *Client
	debounce     time.Duration
	pollInterval time.Duration // <=0 关闭兜底轮询

	mu    sync.Mutex
	subs  map[string][]func()
	timer *time.Timer

	state    atomic.Value // string: starting | connected | reconnecting
	lastSync atomic.Int64 // 最近一次全量刷新的 UnixNano
}

// NewResolver pollInterval <= 0 时关闭兜底轮询。
func NewResolver(cli *Client, pollInterval time.Duration) *Resolver {
	r := &Resolver{
		cli:          cli,
		debounce:     200 * time.Millisecond,
		pollInterval: pollInterval,
		subs:         make(map[string][]func()),
	}
	r.state.Store("starting")
	return r
}

// State 返回事件流状态：starting | connected | reconnecting。
func (r *Resolver) State() string {
	if v := r.state.Load(); v != nil {
		return v.(string)
	}
	return "starting"
}

// LastSyncAt 返回最近一次全量刷新时间（可能为零值）。
func (r *Resolver) LastSyncAt() time.Time {
	return time.Unix(0, r.lastSync.Load())
}

// Watch 订阅容器变化；返回的函数用于退订。回调在事件去抖后触发。
func (r *Resolver) Watch(name string, cb func()) (unwatch func()) {
	r.mu.Lock()
	r.subs[name] = append(r.subs[name], cb)
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		cbs := r.subs[name]
		for i, f := range cbs {
			if reflect.ValueOf(f).Pointer() == reflect.ValueOf(cb).Pointer() {
				r.subs[name] = append(cbs[:i:i], cbs[i+1:]...)
				break
			}
		}
		if len(r.subs[name]) == 0 {
			delete(r.subs, name)
		}
	}
}

// ResolveIP 把容器名解析为当前 IP（委托给 Client）。
func (r *Resolver) ResolveIP(ctx context.Context, name, network string) (ip, networkUsed, containerID string, err error) {
	return r.cli.ResolveIP(ctx, name, network)
}

// Run 事件流主循环：断连后指数退避重连，重连成功先全量刷新补齐丢失事件。
func (r *Resolver) Run(ctx context.Context) {
	if r.pollInterval > 0 {
		go func() {
			t := time.NewTicker(r.pollInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					r.triggerRefresh() // 兜底轮询：与事件路径共用去抖+刷新管道
				}
			}
		}()
	}
	backoff := time.Second
	for {
		err := r.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		r.state.Store("reconnecting")
		slog.Warn("docker event stream disconnected, will reconnect", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (r *Resolver) stream(ctx context.Context) error {
	r.state.Store("connected")
	r.triggerRefresh() // 每次（重）连先全量刷新
	msgs, errs := r.cli.Events(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err == nil {
				continue
			}
			return err
		case _, ok := <-msgs:
			if !ok {
				return errors.New("docker event channel closed")
			}
			r.triggerRefresh()
		}
	}
}

func (r *Resolver) triggerRefresh() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = time.AfterFunc(r.debounce, r.refresh)
}

func (r *Resolver) refresh() {
	r.mu.Lock()
	var cbs []func()
	for _, fs := range r.subs {
		cbs = append(cbs, fs...)
	}
	r.mu.Unlock()
	r.lastSync.Store(time.Now().UnixNano())
	slog.Debug("resolver refresh", "watched", len(cbs))
	for _, cb := range cbs {
		cb()
	}
}
