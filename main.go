package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/collei-monitor/collei-agent/internal/collector"
	"github.com/collei-monitor/collei-agent/internal/config"
	"github.com/collei-monitor/collei-agent/internal/core"
	"github.com/collei-monitor/collei-agent/internal/service"
)

func main() {
	var verbose, debug bool

	rootCmd := &cobra.Command{
		Use:   "collei-agent",
		Short: "Collei Agent - Server monitoring probe",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			setupLogging(verbose, debug)
		},
	}
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose logging")
	rootCmd.PersistentFlags().BoolVar(&debug, "debug", false, "Debug logging")

	// --- run 子命令 ---
	var (
		url          string
		token        string
		regToken     string
		name         string
		configPath   string
		interval     float64
		force        bool
		noAutoUpdate bool
	)
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Start the Agent",
		Run: func(cmd *cobra.Command, args []string) {
			cfg := config.Load(configPath)

			// CLI 标志覆盖配置文件
			if url != "" {
				cfg.ServerURL = url
			}
			if token != "" {
				cfg.Token = token
			}
			if regToken != "" {
				cfg.RegToken = regToken
			}
			if name != "" {
				cfg.Name = name
			}
			if interval > 0 {
				cfg.ReportInterval = interval
			}
			if configPath != "" {
				cfg.ConfigPath = configPath
			}
			if force {
				cfg.ForceRegister = true
			}
			if noAutoUpdate {
				cfg.AutoUpdate = false
			}

			// 验证配置
			if cfg.ServerURL == "" {
				slog.Error("missing server URL, use --url or set server_url in config")
				os.Exit(1)
			}
			if cfg.Token == "" && cfg.RegToken == "" {
				slog.Error("missing auth credentials, use --token or --reg-token")
				os.Exit(1)
			}

			agent := core.New(cfg)

			// Windows 服务模式：在 SCM 下运行
			if service.IsWindowsService() {
				slog.Info("running as Windows service")
				if err := service.RunAsService(func(ctx context.Context) {
					agent.RunWithContext(ctx)
				}); err != nil {
					slog.Error("service run failed", "error", err)
					os.Exit(1)
				}
				return
			}

			agent.Run()
		},
	}
	runCmd.Flags().StringVar(&url, "url", os.Getenv("COLLEI_URL"), "Backend API URL")
	runCmd.Flags().StringVar(&token, "token", os.Getenv("COLLEI_TOKEN"), "Server-specific token (passive registration)")
	runCmd.Flags().StringVar(&regToken, "reg-token", os.Getenv("COLLEI_REG_TOKEN"), "Global registration token (auto registration)")
	runCmd.Flags().StringVar(&name, "name", "", "Server display name")
	runCmd.Flags().StringVar(&configPath, "config", "", "Config file path")
	runCmd.Flags().Float64Var(&interval, "interval", 3.0, "Report interval in seconds")
	runCmd.Flags().BoolVar(&force, "force", false, "Force re-registration")
	runCmd.Flags().BoolVar(&noAutoUpdate, "no-auto-update", false, "Disable automatic version checks")

	// --- collect 子命令 ---
	collectCmd := &cobra.Command{
		Use:   "collect",
		Short: "Test system data collection (no backend connection)",
		Run: func(cmd *cobra.Command, args []string) {
			c := collector.NewSystemCollector(context.Background(), "", "", nil, nil)

			fmt.Println("=== Hardware Info ===")
			hw := c.CollectHardware()
			printJSON(hw)

			fmt.Println("\n=== Load Data (first sample) ===")
			load1 := c.CollectLoad()
			printJSON(load1)

			flowIn, flowOut := c.CollectTotalFlow()
			fmt.Println("\n=== Cumulative Traffic ===")
			fmt.Printf("total_flow_in:  %d\n", flowIn)
			fmt.Printf("total_flow_out: %d\n", flowOut)

			fmt.Println("\nWaiting 2 seconds for second sample...")
			time.Sleep(2 * time.Second)

			fmt.Println("\n=== Load Data (second sample) ===")
			load2 := c.CollectLoad()
			printJSON(load2)
		},
	}

	// --- version 子命令 ---
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Show version info",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("Collei Agent %s\n", core.Version)
			fmt.Printf("Go %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
		},
	}

	// --- service 子命令组 ---
	var serviceConfigPath string
	serviceCmd := &cobra.Command{
		Use:   "service",
		Short: "Manage Windows service (install/uninstall/start/stop)",
	}

	serviceInstallCmd := &cobra.Command{
		Use:   "install",
		Short: "Install as Windows service",
		Run: func(cmd *cobra.Command, args []string) {
			if runtime.GOOS != "windows" {
				slog.Error("service commands are only available on Windows")
				os.Exit(1)
			}
			if err := service.Install(serviceConfigPath); err != nil {
				slog.Error("service install failed", "error", err)
				os.Exit(1)
			}
		},
	}
	serviceInstallCmd.Flags().StringVar(&serviceConfigPath, "config", "", "Config file path for the service")

	serviceUninstallCmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall Windows service",
		Run: func(cmd *cobra.Command, args []string) {
			if err := service.Uninstall(); err != nil {
				slog.Error("service uninstall failed", "error", err)
				os.Exit(1)
			}
		},
	}

	serviceStartCmd := &cobra.Command{
		Use:   "start",
		Short: "Start Windows service",
		Run: func(cmd *cobra.Command, args []string) {
			if err := service.Start(); err != nil {
				slog.Error("service start failed", "error", err)
				os.Exit(1)
			}
		},
	}

	serviceStopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop Windows service",
		Run: func(cmd *cobra.Command, args []string) {
			if err := service.Stop(); err != nil {
				slog.Error("service stop failed", "error", err)
				os.Exit(1)
			}
		},
	}

	serviceCmd.AddCommand(serviceInstallCmd, serviceUninstallCmd, serviceStartCmd, serviceStopCmd)

	rootCmd.AddCommand(runCmd, collectCmd, versionCmd, serviceCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func setupLogging(verbose, debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	var w io.Writer = os.Stderr

	// Windows 服务模式下 stderr 无输出目标，改写入日志文件
	if service.IsWindowsService() {
		logDir := config.ServiceConfigDir()
		_ = os.MkdirAll(logDir, 0o755)
		logPath := filepath.Join(logDir, "agent.log")
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			w = f
		}
	}

	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}

func printJSON(v interface{}) {
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(data))
}
