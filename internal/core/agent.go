package core

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/collei-monitor/collei-agent/internal/api"
	"github.com/collei-monitor/collei-agent/internal/collector"
	"github.com/collei-monitor/collei-agent/internal/config"
	"github.com/collei-monitor/collei-agent/internal/network"
	"github.com/collei-monitor/collei-agent/internal/task"
	"github.com/collei-monitor/collei-agent/internal/tunnel"
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

	running bool
	stopCh  chan struct{}
	once    sync.Once

	apiClient  *api.Client
	collector  *collector.SystemCollector
	netMonitor *network.Monitor
	taskExec   *task.Executor
	sshManager *tunnel.Manager
}

// New 创建一个新的 Agent 实例。
func New(cfg *config.AgentConfig) *Agent {
	stopCh := make(chan struct{})
	stateDir := config.DefaultConfigDir()
	if cfg.ConfigPath != "" {
		stateDir = filepath.Dir(cfg.ConfigPath)
	}

	return &Agent{
		Config:     cfg,
		State:      StateInit,
		stopCh:     stopCh,
		collector:  collector.NewSystemCollector(cfg.NetworkInterface, stateDir),
		netMonitor: network.NewMonitor(stopCh),
	}
}

// Run 启动 Agent 主循环（阻塞直到停止）。
func (a *Agent) Run() {
	a.running = true
	a.setupSignalHandlers()

	slog.Info("Collei Agent started", "version", Version)
	slog.Info("Backend", "url", a.Config.ServerURL)

	a.apiClient = api.NewClient(a.Config.ServerURL, 0)
	a.taskExec = task.NewExecutor(a.apiClient)

	defer a.shutdown()

	a.mainLoop()
}

// Stop 请求 Agent 停止。
func (a *Agent) Stop() {
	slog.Info("Agent stop requested...")
	a.running = false
	a.once.Do(func() { close(a.stopCh) })
}

func (a *Agent) interruptibleSleep(d time.Duration) {
	select {
	case <-a.stopCh:
	case <-time.After(d):
	}
}

func (a *Agent) isStopped() bool {
	select {
	case <-a.stopCh:
		return true
	default:
		return !a.running
	}
}

// --- 主循环 ---

func (a *Agent) mainLoop() {
	a.initialHandshake()

	for a.running {
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

		if a.isStopped() {
			return
		}
	}
}

// --- 注册 / 验证 ---

func (a *Agent) initialHandshake() {
	for a.running {
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

	resp, err := a.apiClient.Register(a.Config.RegToken, name, hw.ToMap(), Version)
	if err != nil {
		return err
	}

	a.Config.UUID = resp.UUID
	a.Config.Token = resp.Token
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

	resp, err := a.apiClient.Verify(a.Config.Token, name, hw.ToMap(), Version)
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

	for a.running && a.State == StateWaitingApproval {
		a.interruptibleSleep(time.Duration(a.Config.VerifyInterval * float64(time.Second)))
		if a.isStopped() {
			return
		}

		resp, err := a.apiClient.Verify(a.Config.Token, "", nil, Version)
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

	for a.running && a.State == StateReporting {
		var networkResults []map[string]interface{}

		func() {
			load := a.collector.CollectLoad()
			hwChanges := a.collector.CollectHardwareIfChanged()
			flowIn, flowOut := a.collector.CollectTotalFlow()
			totalFlowIn, totalFlowOut := &flowIn, &flowOut
			diskIO := a.collector.CollectDiskIO()
			netIO := a.collector.CollectNetIO()
			networkResults = a.netMonitor.FlushPendingResults()

			var networkData []map[string]interface{}
			if len(networkResults) > 0 {
				networkData = networkResults
			}

			var currentDiskIO, currentNetIO interface{}
			if len(diskIO) > 0 {
				currentDiskIO = diskIO
			}
			if len(netIO) > 0 {
				currentNetIO = netIO
			}

			resp, err := a.apiClient.Report(
				a.Config.Token,
				hwChanges,
				load.ToMap(),
				totalFlowIn, totalFlowOut,
				currentDiskIO, currentNetIO,
				a.netMonitor.Version(),
				networkData,
			)
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

func (a *Agent) handleReportError(err error, networkResults []map[string]interface{}) {
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

	apiErr, isApiErr := api.GetApiError(err)
	if isApiErr {
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

func (a *Agent) handleSSHTunnel(sshTunnel map[string]interface{}) {
	if !a.Config.SSH.Enabled || sshTunnel == nil {
		return
	}

	connect, ok := sshTunnel["connect"]
	if !ok {
		return
	}

	if connect == true {
		if a.sshManager == nil {
			a.sshManager = tunnel.NewManager(a.Config.ServerURL, a.Config.Token, a.Config.SSH.Port)
		}
		a.sshManager.Connect()
	} else if connect == false {
		if a.sshManager != nil {
			a.sshManager.Disconnect()
		}
	}
}

// --- 信号处理和清理 ---

func (a *Agent) setupSignalHandlers() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, stopping", "signal", sig)
		a.Stop()
	}()
}

func (a *Agent) shutdown() {
	a.State = StateStopped
	a.netMonitor.Stop()
	if a.taskExec != nil {
		a.taskExec.Shutdown()
		a.taskExec = nil
	}
	if a.sshManager != nil {
		a.sshManager.Stop()
		a.sshManager = nil
	}
	if a.apiClient != nil {
		a.apiClient.Close()
		a.apiClient = nil
	}
	slog.Info("Agent stopped")
}

// --- 工具函数 ---

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

