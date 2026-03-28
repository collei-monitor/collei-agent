package updater

import (
	"log/slog"
	"time"

	"github.com/collei-monitor/collei-agent/internal/config"
)

// DefaultCheckInterval 是自动更新检查的默认间隔。
const DefaultCheckInterval = 6 * time.Hour

// AutoUpdater 定期检查 GitHub Releases 并自动更新 Agent。
type AutoUpdater struct {
	updater        *Updater
	currentVersion string
	interval       time.Duration
	stopCh         chan struct{}
}

// NewAutoUpdater 创建一个新的自动更新器。
func NewAutoUpdater(upd *Updater, currentVersion string) *AutoUpdater {
	return &AutoUpdater{
		updater:        upd,
		currentVersion: currentVersion,
		interval:       DefaultCheckInterval,
		stopCh:         make(chan struct{}),
	}
}

// Start 启动自动更新协程。立即检查一次，之后按 interval 定期检查。
func (a *AutoUpdater) Start() {
	go a.loop()
}

// Stop 停止自动更新协程。
func (a *AutoUpdater) Stop() {
	select {
	case <-a.stopCh:
		// already stopped
	default:
		close(a.stopCh)
	}
}

func (a *AutoUpdater) loop() {
	slog.Info("auto-updater: started", "interval", a.interval, "current_version", a.currentVersion)

	// 启动后延迟一小段时间再首次检查，让 Agent 先完成注册/上报
	if !a.sleep(30 * time.Second) {
		return
	}
	a.check()

	for {
		if !a.sleep(a.interval) {
			return
		}
		a.check()
	}
}

func (a *AutoUpdater) check() {
	slog.Debug("auto-updater: checking for new version")

	release, err := a.updater.CheckGitHubLatest()
	if err != nil {
		slog.Warn("auto-updater: version check failed", "error", err)
		return
	}

	if !NeedsUpdate(a.currentVersion, release.Tag) {
		slog.Debug("auto-updater: already up to date", "version", a.currentVersion)
		return
	}

	slog.Info("auto-updater: new version available",
		"current", a.currentVersion, "latest", release.Tag)

	// 写入升级状态文件（无 ExecutionID 表示自动更新）
	state := &config.UpgradeState{
		TargetVersion:   release.Tag,
		PreviousVersion: a.currentVersion,
		StartedAt:       time.Now().Unix(),
	}
	if err := config.WriteUpgradeState(a.updater.configDir, state); err != nil {
		slog.Error("auto-updater: failed to write upgrade state", "error", err)
		return
	}

	// 执行下载和替换
	if err := a.updater.Upgrade(release.DownloadURL, ""); err != nil {
		slog.Error("auto-updater: upgrade failed", "error", err)
		config.RemoveUpgradeState(a.updater.configDir)
		return
	}

	slog.Info("auto-updater: upgrade successful, triggering restart",
		"from", a.currentVersion, "to", release.Tag)
	TriggerRestart()
}

// sleep 等待指定时间或被停止。返回 false 表示被停止。
func (a *AutoUpdater) sleep(d time.Duration) bool {
	select {
	case <-a.stopCh:
		return false
	case <-time.After(d):
		return true
	}
}
