//go:build !windows

package core

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// setupReloadHandler 启动文件监听 + SIGHUP 信号监听。
// 编辑 agent.yaml 后自动重载；也可通过 kill -HUP 或 systemctl reload 触发。
func (a *Agent) setupReloadHandler() {
	a.startConfigWatcher()
	slog.Info("config file watcher started (edit agent.yaml to hot-reload)")

	// 额外监听 SIGHUP（兼容 systemctl reload）
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ch:
				slog.Info("SIGHUP received, reloading config...")
				a.reloadConfig()
			case <-a.ctx.Done():
				signal.Stop(ch)
				return
			}
		}
	}()
}
