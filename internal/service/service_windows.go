//go:build windows

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceName = "collei-agent"
const serviceDisplayName = "Collei Agent"
const serviceDescription = "Collei server monitoring agent"

// agentSvc 实现 svc.Handler 用于 Windows SCM 集成
type agentSvc struct {
	runFunc func(ctx context.Context)
}

// Execute 由 Windows SCM 调用。启动代理并监听 SCM 停止/关闭命令
func (s *agentSvc) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		s.runFunc(ctx)
	}()

	changes <- svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown,
	}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				slog.Info("SCM 请求停止")
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					slog.Warn("代理关闭超时")
				}
				return false, 0
			}
		case <-done:
			return false, 0
		}
	}
}

// IsWindowsService 报告进程是否作为 Windows 服务运行
func IsWindowsService() bool {
	is, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return is
}

// RunAsService 在 Windows SCM 下运行给定的函数
// runFunc 接收一个上下文，当 SCM 发送停止/关闭时会被取消
func RunAsService(runFunc func(ctx context.Context)) error {
	return svc.Run(serviceName, &agentSvc{runFunc: runFunc})
}

// Install 将 collei-agent 注册为 Windows 服务
func Install(configPath string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM: %w", err)
	}
	defer m.Disconnect()

	// 检查服务是否已存在
	s, err := m.OpenService(serviceName)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %q already exists", serviceName)
	}

	args := []string{"run"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}

	s, err = m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		StartType:   mgr.StartAutomatic,
	}, args...)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// 配置恢复：失败后 5 秒后重新启动
	err = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400) // 重置周期：1 天
	if err != nil {
		slog.Warn("failed to set recovery actions", "error", err)
	}

	fmt.Println("Service installed successfully.")
	return nil
}

// Uninstall 移除 collei-agent Windows 服务
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()

	err = s.Delete()
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}

	fmt.Println("Service uninstalled successfully.")
	return nil
}

// Start 通过 sc.exe 启动 Windows 服务
func Start() error {
	cmd := exec.Command("sc", "start", serviceName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Stop 通过 sc.exe 停止 Windows 服务
func Stop() error {
	cmd := exec.Command("sc", "stop", serviceName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
