package rule

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// LoadRules 读取规则文件（JSON 数组）。
func LoadRules(path string) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rules []Rule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return rules, nil
}

// SaveRules 原子写入规则文件（tmp + fsync + rename）。
func SaveRules(path string, rules []Rule) error {
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WatchRules 监听规则文件变化（含原子替换），去抖后重新加载并回调。
// 加载失败保留旧规则，只记日志。
func WatchRules(ctx context.Context, path string, onChange func([]Rule)) {
	abs, err := filepath.Abs(path)
	if err != nil {
		slog.Error("rules watch disabled", "path", path, "err", err)
		return
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Error("rules watch disabled", "path", abs, "err", err)
		return
	}
	defer w.Close()
	if err := w.Add(filepath.Dir(abs)); err != nil {
		slog.Error("rules watch disabled", "path", abs, "err", err)
		return
	}

	reload := func() {
		rules, err := LoadRules(abs)
		if err != nil {
			slog.Error("rules reload failed, keeping previous", "path", abs, "err", err)
			return
		}
		onChange(rules)
		slog.Info("rules reloaded", "path", abs, "rules", len(rules))
	}

	var timer *time.Timer
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			evAbs, _ := filepath.Abs(ev.Name)
			if evAbs != abs {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(150*time.Millisecond, reload)
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
		}
	}
}
