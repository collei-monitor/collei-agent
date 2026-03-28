package task

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/collei-monitor/collei-agent/internal/api"
	"github.com/collei-monitor/collei-agent/internal/config"
	"github.com/collei-monitor/collei-agent/internal/updater"
)

const (
	MaxOutputLength             = 1 * 1024 * 1024 // 1MB
	IntermediateReportThreshold = 4096
	DefaultMaxWorkers           = 4
	DefaultTimeoutSec           = 300
)

// Executor 处理来自后端的远程任务执行。
type Executor struct {
	apiClient *api.Client
	updater   *updater.Updater

	mu          sync.Mutex
	activeTasks map[string]struct{}

	sem chan struct{} // 工作池信号量
	wg  sync.WaitGroup
}

// NewExecutor 创建一个新的任务执行器。
func NewExecutor(apiClient *api.Client, upd *updater.Updater) *Executor {
	return &Executor{
		apiClient:   apiClient,
		updater:     upd,
		activeTasks: make(map[string]struct{}),
		sem:         make(chan struct{}, DefaultMaxWorkers),
	}
}

// HandlePendingTasks 处理上报响应中的 pending_tasks 列表。
func (e *Executor) HandlePendingTasks(tasks []map[string]interface{}) {
	if len(tasks) == 0 {
		return
	}

	for _, t := range tasks {
		execID, _ := t["execution_id"].(string)
		if execID == "" {
			slog.Warn("task: received task without execution_id, skipping")
			continue
		}

		e.mu.Lock()
		if _, active := e.activeTasks[execID]; active {
			e.mu.Unlock()
			slog.Debug("task: already executing, skipping", "execution_id", execID)
			continue
		}
		e.activeTasks[execID] = struct{}{}
		e.mu.Unlock()

		taskType, _ := t["type"].(string)
		timeoutSec := toIntDefault(t["timeout_sec"], DefaultTimeoutSec)
		slog.Info("task: accepted",
			"execution_id", execID, "type", taskType, "timeout_sec", timeoutSec)

		e.wg.Add(1)
		go func(task map[string]interface{}) {
			e.sem <- struct{}{} // 获取信号量
			defer func() {
				<-e.sem // 释放信号量
				e.wg.Done()
			}()
			e.executeTask(task)
		}(t)
	}
}

// Shutdown 停止接受新任务。不会等待进行中的任务。
func (e *Executor) Shutdown() {
	slog.Info("task: executor shut down")
}

// --- 任务执行 ---

func (e *Executor) executeTask(task map[string]interface{}) {
	execID, _ := task["execution_id"].(string)
	taskType, _ := task["type"].(string)
	timeoutSec := toIntDefault(task["timeout_sec"], DefaultTimeoutSec)

	defer e.finishTask(execID)

	// 解析载荷
	payloadStr, _ := task["payload"].(string)
	var payload map[string]interface{}
	if payloadStr != "" {
		if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
			slog.Error("task: payload parse failed", "execution_id", execID, "error", err)
			e.reportStatus(execID, "failed", intPtr(-1), strPtr(fmt.Sprintf("Payload parse error: %v", err)))
			return
		}
	} else {
		payload = make(map[string]interface{})
	}

	// 上报运行状态
	e.reportStatus(execID, "running", nil, nil)

	switch taskType {
	case "shell", "command":
		e.execShell(execID, payload, timeoutSec)
	case "script":
		e.execScript(execID, payload, timeoutSec)
	case "upgrade_agent":
		e.execUpgrade(execID, payload)
	default:
		e.reportStatus(execID, "failed", intPtr(-1),
			strPtr(fmt.Sprintf("Unsupported task type: %s", taskType)))
	}
}

func (e *Executor) execShell(execID string, payload map[string]interface{}, timeoutSec int) {
	command, _ := payload["command"].(string)
	if command == "" {
		e.reportStatus(execID, "failed", intPtr(-1), strPtr("Empty command"))
		return
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}

	e.runAndStream(execID, cmd, timeoutSec)
}

func (e *Executor) execScript(execID string, payload map[string]interface{}, timeoutSec int) {
	script, _ := payload["script"].(string)
	if script == "" {
		e.reportStatus(execID, "failed", intPtr(-1), strPtr("Empty script"))
		return
	}

	// 写入临时文件
	tmpFile, err := os.CreateTemp("", "collei_task_*.sh")
	if err != nil {
		e.reportStatus(execID, "failed", intPtr(-1), strPtr(fmt.Sprintf("Failed to create temp file: %v", err)))
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(script); err != nil {
		tmpFile.Close()
		e.reportStatus(execID, "failed", intPtr(-1), strPtr(fmt.Sprintf("Failed to write script: %v", err)))
		return
	}
	tmpFile.Close()
	os.Chmod(tmpPath, 0700)

	args := []string{tmpPath}
	if rawArgs, ok := payload["args"].([]interface{}); ok {
		for _, a := range rawArgs {
			args = append(args, fmt.Sprintf("%v", a))
		}
	}

	cmd := exec.Command(args[0], args[1:]...)
	e.runAndStream(execID, cmd, timeoutSec)
}

func (e *Executor) execUpgrade(execID string, payload map[string]interface{}) {
	version, _ := payload["version"].(string)
	downloadURL, _ := payload["url"].(string)
	checksum, _ := payload["checksum"].(string)

	if version == "" || downloadURL == "" {
		e.reportStatus(execID, "failed", intPtr(-1),
			strPtr("Missing required fields: version and url"))
		return
	}

	currentVersion := e.updater.CurrentVersion()

	// 已是目标版本
	if !updater.NeedsUpdate(currentVersion, version) {
		output := fmt.Sprintf("Already at version %s", currentVersion)
		e.reportStatus(execID, "success", intPtr(0), &output)
		return
	}

	// 写入升级状态文件（重启后用于上报结果）
	state := &config.UpgradeState{
		ExecutionID:     execID,
		TargetVersion:   version,
		PreviousVersion: currentVersion,
		StartedAt:       time.Now().Unix(),
	}
	if err := config.WriteUpgradeState(e.updater.ConfigDir(), state); err != nil {
		e.reportStatus(execID, "failed", intPtr(-1),
			strPtr(fmt.Sprintf("Failed to write upgrade state: %v", err)))
		return
	}

	// 执行下载和替换
	if err := e.updater.Upgrade(downloadURL, checksum); err != nil {
		config.RemoveUpgradeState(e.updater.ConfigDir())
		e.reportStatus(execID, "failed", intPtr(-1),
			strPtr(fmt.Sprintf("Upgrade failed: %v", err)))
		return
	}

	output := fmt.Sprintf("Binary replaced, restarting (%s -> %s)", currentVersion, version)
	e.reportStatus(execID, "running", nil, &output)

	// 触发重启（重启后新版本会读取状态文件并上报最终结果）
	updater.TriggerRestart()
}

// runAndStream 执行命令，流式输出并中间上报，处理超时。
func (e *Executor) runAndStream(execID string, cmd *exec.Cmd, timeoutSec int) {
	ctx, cancel := context.WithTimeout(context.Background(), secDuration(timeoutSec))
	defer cancel()

	cmd2 := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	cmd2.Dir = cmd.Dir
	cmd2.Env = cmd.Env

	stdout, err := cmd2.StdoutPipe()
	if err != nil {
		e.reportStatus(execID, "failed", intPtr(-1), strPtr(fmt.Sprintf("Failed to create pipe: %v", err)))
		return
	}
	cmd2.Stderr = cmd2.Stdout // merge stderr into stdout

	if err := cmd2.Start(); err != nil {
		e.reportStatus(execID, "failed", intPtr(-1), strPtr(fmt.Sprintf("Failed to start process: %v", err)))
		return
	}

	var buffer strings.Builder
	totalLength := 0
	buf := make([]byte, 4096)
	truncated := false

	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			if totalLength >= MaxOutputLength {
				if !truncated {
					buffer.WriteString("[... output truncated ...]\n")
					truncated = true
				}
			} else {
				chunk := string(buf[:n])
				buffer.WriteString(chunk)
				totalLength += n

				if buffer.Len() >= IntermediateReportThreshold {
					output := buffer.String()
					e.reportStatus(execID, "running", nil, &output)
					buffer.Reset()
				}
			}
		}
		if readErr != nil {
			break
		}
	}

	err = cmd2.Wait()
	timedOut := ctx.Err() == context.DeadlineExceeded

	remaining := buffer.String()

	if timedOut {
		output := remaining + fmt.Sprintf("\nTask timed out (%ds)", timeoutSec)
		e.reportStatus(execID, "timeout", intPtr(-1), &output)
	} else {
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
		status := "success"
		if exitCode != 0 {
			status = "failed"
		}
		var outputPtr *string
		if remaining != "" {
			outputPtr = &remaining
		}
		e.reportStatus(execID, status, &exitCode, outputPtr)
	}
}

// --- 状态上报 ---

func (e *Executor) reportStatus(execID, status string, exitCode *int, output *string) {
	if err := e.apiClient.ReportTask(execID, status, exitCode, output); err != nil {
		slog.Warn("task: status report failed",
			"execution_id", execID[:min8(len(execID))], "status", status, "error", err)
	} else {
		slog.Debug("task: status reported",
			"execution_id", execID[:min8(len(execID))], "status", status)
	}
}

func (e *Executor) finishTask(execID string) {
	e.mu.Lock()
	delete(e.activeTasks, execID)
	e.mu.Unlock()
}

// --- 辅助函数 ---

func toIntDefault(v interface{}, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return def
	}
}

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }

func min8(n int) int {
	if n < 8 {
		return n
	}
	return 8
}

func secDuration(sec int) time.Duration {
	return time.Duration(sec) * time.Second
}
