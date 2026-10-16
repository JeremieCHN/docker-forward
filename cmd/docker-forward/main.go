package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/JeremieCHN/docker-forward/internal/dockerx"
	"github.com/JeremieCHN/docker-forward/internal/rule"
	"github.com/JeremieCHN/docker-forward/internal/web"
)

var version = "1.0.0"

func main() {
	f := flag.NewFlagSet("docker-forward", flag.ExitOnError)
	f.SetOutput(os.Stderr)
	rulesPath := f.String("rules", "rules.json", "path to rules file")
	dockerHost := f.String("docker", "", "docker host (overrides DOCKER_HOST)")
	webListen := f.String("web", "127.0.0.1:8080", "web api listen addr: `host:port` (tcp) or unix:///path/to.sock")
	token := f.String("token", "", "bearer token for web api")
	pollSec := f.Int("poll-interval", 30, "fallback docker poll interval in seconds (negative disables)")
	debug := f.Bool("debug", false, "debug logging")
	showVersion := f.Bool("version", false, "print version and exit")
	f.Usage = func() {
		fmt.Fprint(f.Output(), `docker-forward - dynamic port forwarding to docker containers

usage:
  docker-forward [flags]

flags:
`)
		f.PrintDefaults()
	}
	f.Parse(os.Args[1:])

	if *showVersion {
		fmt.Println(version)
		return
	}

	setupLog(*debug)

	rules, err := rule.LoadRules(*rulesPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			rules = nil
			slog.Info("rules file not found, starting with empty rules", "path", *rulesPath)
		} else {
			slog.Error("load rules", "err", err)
			os.Exit(1)
		}
	}

	cli, err := dockerx.New(*dockerHost)
	if err != nil {
		slog.Error("connect docker", "err", err)
		os.Exit(1)
	}
	var poll time.Duration
	if *pollSec > 0 {
		poll = time.Duration(*pollSec) * time.Second
	}
	rs := dockerx.NewResolver(cli, poll)
	mgr := rule.NewManager(rs)
	if err := mgr.Apply(rules); err != nil {
		slog.Error("apply initial rules", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go rs.Run(ctx)
	go rule.WatchRules(ctx, *rulesPath, func(rules []rule.Rule) {
		if err := mgr.Apply(rules); err != nil {
			slog.Error("apply reloaded rules rejected, keeping previous", "err", err)
		}
	})

	network, webAddr, err := parseWebListen(*webListen)
	if err != nil {
		slog.Error("parse web listen addr", "addr", *webListen, "err", err)
		os.Exit(1)
	}
	if network == "unix" {
		if err := removeStaleSocket(webAddr); err != nil {
			slog.Error("web listen", "addr", *webListen, "err", err)
			os.Exit(1)
		}
	} else if *token == "" && !isLoopbackListen(webAddr) {
		slog.Warn("web api listens on non-loopback without token, anyone on the network can modify rules", "listen", *webListen)
	}
	srv := web.NewServer(*rulesPath, mgr, cli, rs, *token, version)
	ln, err := net.Listen(network, webAddr)
	if err != nil {
		slog.Error("web listen", "addr", *webListen, "err", err)
		os.Exit(1)
	}
	if network == "unix" {
		// 默认权限受 umask 影响，显式收紧为属主+组可写（连接 socket 需要写权限）
		if err := os.Chmod(webAddr, 0o660); err != nil {
			slog.Error("chmod web socket", "err", err)
			ln.Close()
			os.Exit(1)
		}
	}
	go func() {
		if err := http.Serve(ln, srv.Handler()); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("web serve", "err", err)
		}
	}()

	slog.Info("docker-forward started", "version", version, "rulesFile", *rulesPath, "rules", len(rules), "web", *webListen)
	<-ctx.Done()
	ln.Close()
	if network == "unix" {
		os.Remove(webAddr)
	}
	mgr.Shutdown()
	slog.Info("docker-forward stopped")
}

func setupLog(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseWebListen 解析 -web 参数：unix:///path/to.sock（或 unix:/path）走 unix socket，其余按 tcp host:port。
func parseWebListen(s string) (network, addr string, err error) {
	if rest, ok := strings.CutPrefix(s, "unix://"); ok {
		return "unix", rest, nil
	}
	if rest, ok := strings.CutPrefix(s, "unix:"); ok {
		return "unix", rest, nil
	}
	return "tcp", s, nil
}

// removeStaleSocket 清理上次异常退出残留的 socket 文件；若路径上是非 socket 文件则拒绝删除。
func removeStaleSocket(path string) error {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket file %s", path)
	}
	return os.Remove(path)
}
