package dockerx

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// Client 是对 Docker SDK 的窄封装：只暴露本工具需要的能力。
type Client struct {
	cli *client.Client
}

func New(host string) (*Client, error) {
	opts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if host != "" {
		opts = append(opts, client.WithHost(host))
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		return nil, fmt.Errorf("docker ping failed: %w (需要能访问 docker daemon：挂载/存在 docker.sock 且当前用户有权限)", err)
	}
	return &Client{cli: cli}, nil
}

// Host 返回当前连接的 docker daemon 地址（用于状态展示）。
func (c *Client) Host() string { return c.cli.DaemonHost() }

// Ping 检测 daemon 连通性。
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx)
	return err
}

// ContainerID 按名称/ID 取容器完整 ID（不要求运行中）。
func (c *Client) ContainerID(ctx context.Context, name string) (string, error) {
	ins, err := c.cli.ContainerInspect(ctx, name)
	if err != nil {
		return "", err
	}
	return ins.ID, nil
}

// ContainerInfo 是容器列表条目（供 WebUI 选择器使用）。
type ContainerInfo struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Image  string            `json:"image"`
	State  string            `json:"state"`
	Status string            `json:"status"`
	IPs    map[string]string `json:"ips,omitempty"` // network -> IP
	Ports  []string          `json:"ports,omitempty"`
}

// ListContainers 返回运行中的容器。
func (c *Client) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	list, err := c.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerInfo, 0, len(list))
	for _, ct := range list {
		info := ContainerInfo{
			ID:     shortID(ct.ID),
			State:  ct.State,
			Status: ct.Status,
			Image:  ct.Image,
		}
		if len(ct.Names) > 0 {
			info.Name = strings.TrimPrefix(ct.Names[0], "/")
		}
		for n, ep := range ct.NetworkSettings.Networks {
			if ep.IPAddress == "" {
				continue
			}
			if info.IPs == nil {
				info.IPs = map[string]string{}
			}
			info.IPs[n] = ep.IPAddress
		}
		seen := map[string]bool{}
		var ports []container.Port
		for _, p := range ct.Ports {
			// 同一发布端口会有 IPv4/IPv6 两条映射，按 (pub, priv, proto) 去重
			k := fmt.Sprintf("%d/%d/%s", p.PublicPort, p.PrivatePort, p.Type)
			if seen[k] {
				continue
			}
			seen[k] = true
			ports = append(ports, p)
		}
		// API 返回顺序不稳定，排序避免 WebUI 刷新时跳动；已发布端口排在仅暴露端口前
		sort.Slice(ports, func(i, j int) bool {
			a, b := ports[i], ports[j]
			if (a.PublicPort != 0) != (b.PublicPort != 0) {
				return a.PublicPort != 0
			}
			if a.PublicPort != b.PublicPort {
				return a.PublicPort < b.PublicPort
			}
			if a.PrivatePort != b.PrivatePort {
				return a.PrivatePort < b.PrivatePort
			}
			return a.Type < b.Type
		})
		for _, p := range ports {
			info.Ports = append(info.Ports, formatPort(p))
		}
		out = append(out, info)
	}
	return out, nil
}

// ResolveIP 把容器名解析为当前 IP。
// network 为空时取"网络名排序第一且有 IP"的网络；返回实际使用的网络名。
func (c *Client) ResolveIP(ctx context.Context, name, network string) (ip, networkUsed, containerID string, err error) {
	ins, err := c.cli.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", "", "", fmt.Errorf("container %q not found", name)
		}
		return "", "", "", fmt.Errorf("inspect %q: %w", name, err)
	}
	if ins.State == nil || !ins.State.Running {
		state := "unknown"
		if ins.State != nil {
			state = ins.State.Status
		}
		return "", "", "", fmt.Errorf("container %q is not running (%s)", name, state)
	}
	if ins.NetworkSettings == nil || len(ins.NetworkSettings.Networks) == 0 {
		return "", "", "", fmt.Errorf("container %q has no network", name)
	}
	networks := ins.NetworkSettings.Networks
	if network != "" {
		ep, ok := networks[network]
		if !ok {
			return "", "", "", fmt.Errorf("container %q is not attached to network %q", name, network)
		}
		if ep.IPAddress == "" {
			return "", "", "", fmt.Errorf("container %q has no IP on network %q", name, network)
		}
		return ep.IPAddress, network, ins.ID, nil
	}
	names := make([]string, 0, len(networks))
	for n, ep := range networks {
		if ep.IPAddress != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "", "", "", fmt.Errorf("container %q has no IP on any network", name)
	}
	sort.Strings(names)
	return networks[names[0]].IPAddress, names[0], ins.ID, nil
}

// Events 返回容器/网络相关事件流。
func (c *Client) Events(ctx context.Context) (<-chan events.Message, <-chan error) {
	f := filters.NewArgs()
	f.Add("type", "container")
	f.Add("type", "network")
	for _, a := range []string{
		"start", "die", "stop", "kill", "restart", "destroy", "pause", "unpause",
		"connect", "disconnect",
	} {
		f.Add("event", a)
	}
	return c.cli.Events(ctx, events.ListOptions{Filters: f})
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func formatPort(p container.Port) string {
	if p.PublicPort != 0 {
		return fmt.Sprintf("%d->%d/%s", p.PublicPort, p.PrivatePort, p.Type)
	}
	return fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
}
