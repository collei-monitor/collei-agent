package core

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"

	"github.com/collei-monitor/collei-agent/internal/api"
	"github.com/collei-monitor/collei-agent/internal/auth"
	"github.com/collei-monitor/collei-agent/internal/collector"
	"github.com/collei-monitor/collei-agent/internal/config"
	"github.com/collei-monitor/collei-agent/internal/fileapi"
	"github.com/collei-monitor/collei-agent/internal/network"
	"github.com/collei-monitor/collei-agent/internal/task"
	"github.com/collei-monitor/collei-agent/internal/terminal"
	"github.com/collei-monitor/collei-agent/internal/tunnel"
	"github.com/collei-monitor/collei-agent/internal/updater"
)

// Version 在编译时通过 ldflags 设置。
var Version = "dev"

// AgentState 表示 Agent 生命周期状态。
type AgentState int

const (
	StateInit AgentState = iota
	StateRegistering
	StateVerifying
	StateWaitingApproval
	StateReporting
	StateStopped
)

func (s AgentState) String() string {
	switch s {
	case StateInit:
		return "INIT"
	case StateRegistering:
		return "REGISTERING"
	case StateVerifying:
		return "VERIFYING"
	case StateWaitingApproval:
		return "WAITING_APPROVAL"
	case StateReporting:
		return "REPORTING"
	case StateStopped:
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}

// Agent 是管理完整生命周期的核心控制器。
type Agent struct {
	Config *config.AgentConfig
	State  AgentState

	ctx    context.Context
	cancel context.CancelFunc

	apiClient     *api.Client
	collector     *collector.SystemCollector
	netMonitor    *network.Monitor
	taskExec      *task.Executor
	sshManager    *tunnel.Manager
	termManager   *terminal.Manager
	fileManager   *fileapi.Manager
	verifier      *auth.Verifier
	autoUpdater   *updater.AutoUpdater
	configWatcher *fsnotify.Watcher
	stateDir      string
}

// New 创建一个新的 Agent 实例。
func New(cfg *config.AgentConfig) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	stateDir := config.DefaultConfigDir()
	if cfg.ConfigPath != "" {
		stateDir = filepath.Dir(cfg.ConfigPath)
	}

	return &Agent{
		Config:     cfg,
		State:      StateInit,
		ctx:        ctx,
		cancel:     cancel,
		stateDir:   stateDir,
		collector:  collector.NewSystemCollector(ctx, cfg.NetworkInterface, stateDir, cfg.NICFilter.Whitelist, cfg.NICFilter.Blacklist),
		netMonitor: network.NewMonitor(ctx),
	}
}

// Run 启动 Agent 主循环（阻塞直到停止）。
func (a *Agent) Run() {
	a.setupSignalHandlers()
	a.setupReloadHandler()

	slog.Info("Collei Agent started", "version", Version)

	// 将国际化域名（如中文域名）转换为 Punycode
	a.Config.ServerURL = config.NormalizeIDNURL(a.Config.ServerURL)

	// 确保 run_id 存在（首次安装或强制注册时生成）
	a.ensureRunID()

	slog.Info("Backend", "url", a.Config.ServerURL)

	a.apiClient = api.NewClient(a.Config.ServerURL, 0)

	upd := updater.NewUpdater(a.stateDir, a.Config.ServerURL, a.Config.Token, Version)
	a.taskExec = task.NewExecutor(a.apiClient, upd, a.Config.TasksEnabled())

	if !a.Config.TasksEnabled() {
		slog.Info("remote task execution disabled (SSH not enabled)")
	}

	// 检查上次升级结果
	a.checkUpgradeResult(upd)

	// 加载 CA 公钥验证器（用于终端/文件 API 签名验证）
	a.loadVerifier()

	// 启动自动更新
	if a.Config.AutoUpdate {
		a.autoUpdater = updater.NewAutoUpdater(a.ctx, upd, Version)
		a.autoUpdater.Start()
		slog.Info("auto-update enabled", "interval", updater.DefaultCheckInterval)
	} else {
		slog.Info("auto-update disabled")
	}

	defer a.shutdown()

	a.mainLoop()
}

// Stop 请求 Agent 停止。
func (a *Agent) Stop() {
	slog.Info("Agent stop requested...")
	a.cancel()
}

// RunWithContext 使用外部上下文启动代理（适用于 Windows 服务模式）
// 外部上下文替换内部取消上下文
func (a *Agent) RunWithContext(ctx context.Context) {
	a.cancel() // 取消原始内部上下文
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.Run()
}

// ensureRunID 确保 run_id 存在。首次安装或强制注册时生成新 UUID。
func (a *Agent) ensureRunID() {
	if a.Config.ForceRegister {
		// 强制注册模式：在 register 成功后生成新的 run_id，此处先清空
		a.Config.RunID = ""
		return
	}
	if a.Config.RunID != "" {
		slog.Info("using existing run_id", "run_id", a.Config.RunID)
		return
	}
	a.Config.RunID = uuid.New().String()
	slog.Info("generated new run_id", "run_id", a.Config.RunID)
	a.Config.Save(a.Config.ConfigPath)
}

func (a *Agent) loadVerifier() {
	caPath := a.Config.CAPublicKeyPath
	if caPath == "" {
		caPath = config.DefaultCAPublicKeyPath()
	}

	v, err := auth.LoadVerifierFromFile(caPath)
	if err != nil {
		slog.Warn("CA public key load failed, signature verification disabled", "path", caPath, "error", err)
		return
	}
	if v == nil {
		slog.Info("CA public key not found, signature verification disabled", "path", caPath)
		return
	}

	a.verifier = v
	slog.Info("CA signature verification enabled", "path", caPath)
}

func (a *Agent) interruptibleSleep(d time.Duration) {
	select {
	case <-a.ctx.Done():
	case <-time.After(d):
	}
}

func (a *Agent) isStopped() bool {
	select {
	case <-a.ctx.Done():
		return true
	default:
		return false
	}
}

// --- 主循环 ---

func (a *Agent) mainLoop() {
	a.initialHandshake()

	for {
		if a.isStopped() {
			return
		}

		switch a.State {
		case StateWaitingApproval:
			a.pollApproval()
		case StateReporting:
			a.reportLoop()
		case StateStopped:
			return
		default:
			slog.Error("unexpected state, stopping", "state", a.State)
			return
		}
	}
}

// --- 注册 / 验证 ---

func (a *Agent) initialHandshake() {
	for {
		if a.isStopped() {
			return
		}

		var err error
		switch {
		case a.Config.ForceRegister && a.Config.RegToken != "":
			if a.Config.IsRegistered() {
				slog.Info("--force detected with existing config, trying verify first...")
				err = a.doVerify()
				if err != nil {
					if api.IsTokenInvalid(err) {
						slog.Warn("local token invalid, forcing re-register")
						err = a.doRegister()
					}
				}
			} else {
				slog.Info("force register mode enabled")
				err = a.doRegister()
			}

		case a.Config.IsRegistered():
			err = a.doVerify()

		case a.Config.RegToken != "":
			err = a.doRegister()

		case a.Config.Token != "":
			err = a.doVerify()

		default:
			slog.Error("no auth credentials available (need --token or --reg-token)")
			a.State = StateStopped
			return
		}

		if err == nil {
			return
		}

		if api.IsTokenInvalid(err) {
			slog.Error("auth failed: token/key invalid, check config")
			a.State = StateStopped
			return
		}

		// run_id 冲突：有限次重试（可能是正常重启，等待后端标记旧实例离线）
		if api.IsTokenConflict(err) {
			slog.Warn("token conflict detected (another agent instance using this token), retrying...")
			resolved := false
			for i := 1; i <= 5; i++ {
				a.interruptibleSleep(5 * time.Second)
				if a.isStopped() {
					return
				}
				slog.Info("conflict retry", "attempt", i)
				retryErr := a.doVerify()
				if retryErr == nil {
					resolved = true
					break
				}
				if !api.IsTokenConflict(retryErr) {
					// 不同类型的错误，回到主循环处理
					err = retryErr
					break
				}
			}
			if resolved {
				return
			}
			if api.IsTokenConflict(err) {
				slog.Error("token conflict persists after 5 retries, another agent is using this token — exiting")
				a.State = StateStopped
				return
			}
			// 非冲突错误，继续主循环
		}

		// 网络或 API 错误 → 重试
		slog.Warn("handshake failed, retrying", "error", err)
		a.interruptibleSleep(5 * time.Second)
	}
}

func (a *Agent) doRegister() error {
	a.State = StateRegistering
	slog.Info("auto-registering (global key mode)...")

	hw := a.collector.CollectHardware()
	name := a.Config.Name
	if name == "" {
		name = defaultHostname()
	}

	resp, err := a.apiClient.Register(a.Config.RegToken, name, hw, Version)
	if err != nil {
		return err
	}

	a.Config.UUID = resp.UUID
	a.Config.Token = resp.Token

	// 注册成功后生成新的 run_id
	a.Config.RunID = uuid.New().String()
	slog.Info("generated new run_id after registration", "run_id", a.Config.RunID)

	a.Config.Save(a.Config.ConfigPath)

	slog.Info("registration successful", "uuid", resp.UUID)
	slog.Info("server pending approval, entering wait mode")
	a.State = StateWaitingApproval
	return nil
}

func (a *Agent) doVerify() error {
	a.State = StateVerifying
	slog.Info("verifying identity...")

	hw := a.collector.CollectHardware()
	name := a.Config.Name
	if name == "" {
		name = defaultHostname()
	}

	resp, err := a.apiClient.Verify(a.Config.Token, a.Config.RunID, name, hw, Version)
	if err != nil {
		return err
	}

	a.Config.UUID = resp.UUID
	a.Config.Token = resp.Token
	a.Config.Save(a.Config.ConfigPath)

	slog.Info("verification successful", "uuid", resp.UUID, "is_approved", resp.IsApproved)

	if resp.IsApproved == 1 {
		a.netMonitor.HandleDispatch(resp.NetworkDispatch)
		a.State = StateReporting
	} else {
		slog.Info("server not yet approved, entering wait mode")
		a.State = StateWaitingApproval
	}
	return nil
}

// --- 审批轮询 ---

func (a *Agent) pollApproval() {
	slog.Info("waiting for approval", "poll_interval_sec", a.Config.VerifyInterval)

	for !a.isStopped() && a.State == StateWaitingApproval {
		a.interruptibleSleep(time.Duration(a.Config.VerifyInterval * float64(time.Second)))
		if a.isStopped() {
			return
		}

		resp, err := a.apiClient.Verify(a.Config.Token, a.Config.RunID, "", nil, Version)
		if err != nil {
			if api.IsTokenInvalid(err) {
				slog.Error("token invalid, stopping")
				a.State = StateStopped
				return
			}
			slog.Warn("approval poll failed", "error", err)
			continue
		}

		if resp.IsApproved == 1 {
			slog.Info("server approved! starting data reporting")
			a.State = StateReporting
			return
		}
		slog.Debug("still waiting for approval...")
	}
}

// --- 上报循环 ---

func (a *Agent) reportLoop() {
	slog.Info("entering report loop", "interval_sec", a.Config.ReportInterval)

	// 初始化网络计数器并等待 CPU 采样
	a.collector.CollectLoad()
	waitTime := 1.0
	if a.Config.ReportInterval < waitTime {
		waitTime = a.Config.ReportInterval
	}
	a.interruptibleSleep(time.Duration(waitTime * float64(time.Second)))

	for !a.isStopped() && a.State == StateReporting {
		var networkResults []network.ProbeResult

		func() {
			load := a.collector.CollectLoad()
			hwChanges := a.collector.CollectHardwareIfChanged()
			flowIn, flowOut := a.collector.CollectTotalFlow()
			networkResults = a.netMonitor.FlushPendingResults()

			resp, err := a.apiClient.Report(&api.ReportParams{
				Token:          a.Config.Token,
				RunID:          a.Config.RunID,
				Hardware:       hwChanges,
				LoadData:       load,
				TotalFlowIn:    flowIn,
				TotalFlowOut:   flowOut,
				DiskIO:         a.collector.CollectDiskIO(),
				NetIO:          a.collector.CollectNetIO(),
				NetworkVersion: a.netMonitor.Version(),
				NetworkData:    networkResults,
				Features: &api.AgentFeatures{
					SSHEnabled:      a.Config.SSH.Enabled,
					TerminalEnabled: a.Config.Terminal.Enabled,
					FileAPIEnabled:  a.Config.FileAPI.Enabled,
					TasksEnabled:    a.Config.TasksEnabled(),
				},
			})
			if err != nil {
				a.handleReportError(err, networkResults)
				return
			}

			if resp.Received {
				a.collector.ConfirmNetReported()
				slog.Debug("report success",
					"cpu", fmt.Sprintf("%.1f%%", load.CPU),
					"ram", load.RAM,
					"net_in", load.NetIn,
					"net_out", load.NetOut)
			}

			a.netMonitor.HandleDispatch(resp.NetworkDispatch)
			a.handleSSHTunnel(resp.SSHTunnel)
			a.handleTerminal(resp.Terminal)
			a.handleFileAPI(resp.FileAPI)

			if a.taskExec != nil {
				a.taskExec.HandlePendingTasks(resp.PendingTasks)
			}
		}()

		if a.isStopped() || a.State != StateReporting {
			return
		}

		a.interruptibleSleep(time.Duration(a.Config.ReportInterval * float64(time.Second)))
	}
}

func (a *Agent) handleReportError(err error, networkResults []network.ProbeResult) {
	if api.IsServerNotApproved(err) {
		slog.Warn("server approval revoked, returning to wait mode")
		a.State = StateWaitingApproval
		return
	}

	if api.IsTokenInvalid(err) {
		slog.Error("token invalid, stopping reporting")
		a.State = StateStopped
		return
	}

	// run_id 冲突：有限次重试
	if api.IsTokenConflict(err) {
		slog.Warn("token conflict during report, retrying...")
		a.netMonitor.RequeueResults(networkResults)
		for i := 1; i <= 5; i++ {
			a.interruptibleSleep(5 * time.Second)
			if a.isStopped() {
				return
			}
			slog.Info("conflict retry (report)", "attempt", i)
			// 用 verify 重新握手
			retryErr := a.doVerify()
			if retryErr == nil {
				return // 冲突解决，继续 reportLoop
			}
			if !api.IsTokenConflict(retryErr) {
				// 非冲突错误
				slog.Error("non-conflict error during retry", "error", retryErr)
				if api.IsTokenInvalid(retryErr) {
					a.State = StateStopped
				}
				return
			}
		}
		slog.Error("token conflict persists after 5 retries during report — exiting")
		a.State = StateStopped
		return
	}

	apiErr, isAPIErr := api.GetAPIError(err)
	if isAPIErr {
		if apiErr.StatusCode >= 500 {
			slog.Warn("server error, retrying", "status", apiErr.StatusCode, "detail", apiErr.Detail)
			a.netMonitor.RequeueResults(networkResults)
			a.interruptibleSleep(5 * time.Second)
			return
		}
		if apiErr.StatusCode == 429 {
			retryAfter := parseRetryAfter(apiErr.Headers)
			if retryAfter > 0 {
				slog.Warn("rate limited, waiting", "retry_after_sec", retryAfter)
				a.interruptibleSleep(time.Duration(retryAfter * float64(time.Second)))
			} else {
				slog.Warn("rate limited, retrying")
				a.interruptibleSleep(5 * time.Second)
			}
			a.netMonitor.RequeueResults(networkResults)
			return
		}
		slog.Error("report error", "status", apiErr.StatusCode, "detail", apiErr.Detail)
		a.netMonitor.RequeueResults(networkResults)
		a.interruptibleSleep(5 * time.Second)
		return
	}

	// 网络错误
	slog.Warn("report failed (network)", "error", err)
	a.netMonitor.RequeueResults(networkResults)
	a.interruptibleSleep(5 * time.Second)
}

// --- SSH 隧道 ---

func (a *Agent) handleSSHTunnel(directive *api.SSHTunnelDirective) {
	if !a.Config.SSH.Enabled || directive == nil || directive.Connect == nil {
		return
	}

	if *directive.Connect {
		if a.sshManager == nil {
			a.sshManager = tunnel.NewManager(a.ctx, a.Config.ServerURL, a.Config.Token, a.Config.SSH.Port)
		}
		a.sshManager.Connect()
	} else {
		if a.sshManager != nil {
			a.sshManager.Disconnect()
		}
	}
}

// --- 终端直连（ConPTY） ---

func (a *Agent) handleTerminal(directive *api.TerminalDirective) {
	if !a.Config.Terminal.Enabled || directive == nil || directive.Connect == nil {
		// 自动启用：Windows 平台且 terminal 支持时，即使未在配置中显式启用
		if runtime.GOOS == "windows" && terminal.Supported() && directive != nil && directive.Connect != nil {
			// allow
		} else {
			return
		}
	}

	if *directive.Connect {
		if a.termManager == nil {
			a.termManager = terminal.NewManager(
				a.ctx,
				a.Config.ServerURL,
				a.Config.Token,
				a.Config.Terminal.DefaultShell,
				a.verifier,
			)
		}
		a.termManager.Connect()
	} else {
		if a.termManager != nil {
			a.termManager.Disconnect()
		}
	}
}

// --- 文件 API ---

func (a *Agent) handleFileAPI(directive *api.FileAPIDirective) {
	if !a.Config.FileAPI.Enabled || directive == nil || directive.Connect == nil {
		if runtime.GOOS == "windows" && directive != nil && directive.Connect != nil {
			// allow on Windows even if not explicitly configured
		} else {
			return
		}
	}

	if *directive.Connect {
		if a.fileManager == nil {
			a.fileManager = fileapi.NewManager(
				a.ctx,
				a.Config.ServerURL,
				a.Config.Token,
				a.verifier,
			)
		}
		a.fileManager.Connect()
	} else {
		if a.fileManager != nil {
			a.fileManager.Disconnect()
		}
	}
}

// --- 信号处理和清理 ---

func (a *Agent) setupSignalHandlers() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case sig := <-sigCh:
			slog.Info("received signal, stopping", "signal", sig)
			a.cancel()
		case <-a.ctx.Done():
		}
		signal.Stop(sigCh)
	}()
}

func (a *Agent) shutdown() {
	a.State = StateStopped
	a.netMonitor.Stop()
	if a.configWatcher != nil {
		a.configWatcher.Close()
		a.configWatcher = nil
	}
	if a.autoUpdater != nil {
		a.autoUpdater.Stop()
		a.autoUpdater = nil
	}
	if a.taskExec != nil {
		a.taskExec.Shutdown()
		a.taskExec = nil
	}
	if a.sshManager != nil {
		a.sshManager.Stop()
		a.sshManager = nil
	}
	if a.termManager != nil {
		a.termManager.Stop()
		a.termManager = nil
	}
	if a.fileManager != nil {
		a.fileManager.Stop()
		a.fileManager = nil
	}
	if a.apiClient != nil {
		a.apiClient.Close()
		a.apiClient = nil
	}
	slog.Info("Agent stopped")
}

// --- 配置文件监听 ---

// startConfigWatcher 启动 fsnotify 文件监听，配置文件变更后自动重载。
func (a *Agent) startConfigWatcher() {
	cfgPath := a.Config.ConfigPath
	if cfgPath == "" {
		cfgPath = config.DefaultConfigPath()
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("failed to create config watcher", "error", err)
		return
	}
	a.configWatcher = watcher

	// 监听配置文件所在目录（fsnotify 要求监听目录以捕获 rename/create 事件）
	dir := filepath.Dir(cfgPath)
	base := filepath.Base(cfgPath)
	if err := watcher.Add(dir); err != nil {
		slog.Warn("failed to watch config directory", "dir", dir, "error", err)
		watcher.Close()
		a.configWatcher = nil
		return
	}

	go func() {
		// 去抖动：多次快速写入只触发一次重载
		var debounce *time.Timer
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// 仅关注目标配置文件
				if filepath.Base(event.Name) != base {
					continue
				}
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					// 去抖动：500ms 内的多次写入合并为一次重载
					if debounce != nil {
						debounce.Stop()
					}
					debounce = time.AfterFunc(500*time.Millisecond, func() {
						slog.Info("config file changed, reloading...", "path", cfgPath)
						a.reloadConfig()
					})
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				slog.Warn("config watcher error", "error", err)
			case <-a.ctx.Done():
				return
			}
		}
	}()
}

// reloadConfig 执行配置热重载。
func (a *Agent) reloadConfig() {
	if err := a.Config.ReloadHotFields(); err != nil {
		slog.Error("config reload failed", "error", err)
		return
	}
	a.collector.UpdateNICFilter(
		a.Config.NICFilter.Whitelist,
		a.Config.NICFilter.Blacklist,
	)
	slog.Info("config reload complete")
}

// --- 工具函数 ---

// checkUpgradeResult 检查上次升级操作的结果并上报。
func (a *Agent) checkUpgradeResult(upd *updater.Updater) {
	state, err := config.ReadUpgradeState(upd.ConfigDir())
	if err != nil {
		slog.Warn("failed to read upgrade state", "error", err)
		return
	}
	if state == nil {
		return
	}

	defer config.RemoveUpgradeState(upd.ConfigDir())

	const expireSeconds = 600 // 10 分钟
	elapsed := time.Now().Unix() - state.StartedAt

	var status string
	var exitCode int
	var output string

	switch {
	case elapsed > expireSeconds:
		status = "failed"
		exitCode = -1
		output = fmt.Sprintf("Upgrade state expired (started %ds ago)", elapsed)
	case Version == state.TargetVersion:
		status = "success"
		exitCode = 0
		output = fmt.Sprintf("Upgraded from %s to %s", state.PreviousVersion, state.TargetVersion)
	default:
		status = "failed"
		exitCode = -1
		output = fmt.Sprintf("Version mismatch: expected %s, got %s", state.TargetVersion, Version)
	}

	slog.Info("upgrade result", "status", status, "detail", output)

	// 仅当有 ExecutionID 时上报给后端（面板任务触发的升级）
	if state.ExecutionID != "" && a.apiClient != nil {
		if err := a.apiClient.ReportTask(state.ExecutionID, status, &exitCode, &output); err != nil {
			slog.Warn("failed to report upgrade result", "error", err)
		}
	}
}

func defaultHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func parseRetryAfter(headers http.Header) float64 {
	value := headers.Get("Retry-After")
	if value == "" {
		return 0
	}
	// Try numeric seconds
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		if f > 0 {
			return f
		}
		return 0
	}
	// Try HTTP date
	if t, err := http.ParseTime(value); err == nil {
		delta := time.Until(t).Seconds()
		if delta > 0 {
			return delta
		}
	}
	return 0
}
