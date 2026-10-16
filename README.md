# docker-forward

运行在 Docker 宿主机上的动态端口转发工具（TCP/UDP，用户态代理）。

- WebUI 动态管理转发规则，实时生效
- 转发目标限定为 Docker 容器：列表选择或手动输入容器名
- 监听 Docker 事件流，容器重启/重建导致 IP 变化时自动热更新转发目标
- 事件流断连自动重连 + 周期性兜底轮询，双保险

## 快速开始

```bash
go build -o docker-forward ./cmd/docker-forward
./docker-forward
```

打开 WebUI：http://127.0.0.1:8080 （默认监听回环地址）。

如需彻底关闭 TCP 暴露面，可改用 unix socket 并通过 SSH 转发访问：

```bash
# 宿主机上
docker-forward -web unix:///run/docker-forward/web.sock
# 本地
ssh -L 8080:/run/docker-forward/web.sock user@host
# 浏览器打开 http://127.0.0.1:8080
```

## CLI

```
docker-forward [-rules path] [-docker host] [-web addr] [-token t] [-poll-interval sec] [-debug]
docker-forward -version
```

默认读取 `./rules.json`（`-rules` 覆盖），手工修改规则文件也会实时生效。

## 规则文件

JSON 数组，写入采用 tmp+rename 原子替换：

```json
[
  {
    "id": "r1", "name": "redis", "protocol": "tcp",
    "listenAddr": "0.0.0.0", "listenPort": 16379,
    "containerName": "my-redis", "network": "", "targetPort": 6379,
    "enabled": true, "closeOnSwitch": false, "udpIdleTimeoutSec": 60
  }
]
```

- `-poll-interval`：兜底全量刷新周期（秒），默认 30，负数关闭
- `-web`：默认 `127.0.0.1:8080`；也支持 `unix:///path/to.sock`（socket 权限 0660，不暴露任何 TCP 端口）
- `-token`：非空时 API 需要 `Authorization: Bearer <token>`；WebUI 会弹窗输入
- `network` 仅在容器多网络时指定，默认自动取第一个有 IP 的网络

## REST API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET/POST | `/api/rules` | 列表（含运行时状态）/ 创建 |
| GET/PUT/DELETE | `/api/rules/{id}` | 查询 / 更新 / 删除 |
| POST | `/api/rules/{id}/enable` `/disable` | 启停 |
| GET | `/api/containers` | 运行中容器列表 |
| GET | `/api/status` | docker 连通性、事件流状态、规则汇总 |

写操作全部「校验 → 原子落盘 → 实时应用」。

## 部署

**二进制 + systemd**（示例见 [deploy/docker-forward.service](deploy/docker-forward.service)）：

```bash
sudo install docker-forward /usr/local/bin/
sudo install -m 644 deploy/docker-forward.service /etc/systemd/system/
sudo mkdir -p /etc/docker-forward && sudo cp rules.json /etc/docker-forward/ 2>/dev/null || true
sudo systemctl enable --now docker-forward
```

**容器化**（host 网络模式，与宿主机进程等价）：

```bash
docker build -t docker-forward .
docker run -d --name docker-forward --restart unless-stopped \
  --network host \
  -e DOCKER_FORWARD_SELF=docker-forward \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /etc/docker-forward:/etc/docker-forward \
  docker-forward
```

- 挂载 `docker.sock` 等价于授予宿主机 root 权限。
- 规则文件要按**目录**挂载（而不是单文件），否则原子写入（rename）后的新文件不会同步回宿主机。
- `--network host` 下容器 hostname 与宿主机相同，自引用防护依赖 `DOCKER_FORWARD_SELF` 显式声明自身容器名；不设置则失去该防护（规则目标指向自身会因无桥接 IP 而进入 error 状态，不会形成转发环路）。

绑定 <1024 端口需要 root 或 `CAP_NET_BIND_SERVICE`。

## 许可证

[MIT](LICENSE)
