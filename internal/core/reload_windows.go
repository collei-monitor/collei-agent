//go:build windows

package core

import (
	"log/slog"
)

// setupReloadHandler 在 Windows 上仅启动文件监听。
// Windows 不支持 SIGHUP 信号。
func (a *Agent) setupReloadHandler() {
	a.startConfigWatcher()
	slog.Info("config file watcher started (edit agent.yaml to hot-reload)")
}
