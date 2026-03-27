package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// SSHConfig 存储 SSH 隧道配置。
type SSHConfig struct {
	Enabled      bool `yaml:"enabled"`
	Port         int  `yaml:"port"`
	CaConfigured bool `yaml:"ca_configured"`
}

// persistedConfig 是写入 agent.yaml 的字段子集。
type persistedConfig struct {
	ServerURL        string     `yaml:"server_url,omitempty"`
	UUID             string     `yaml:"uuid,omitempty"`
	Token            string     `yaml:"token,omitempty"`
	NetworkInterface string     `yaml:"network_interface,omitempty"`
	SSH              *SSHConfig `yaml:"ssh,omitempty"`
}

// AgentConfig 存储完整的 Agent 配置。
type AgentConfig struct {
	// 持久化字段
	ServerURL        string
	UUID             string
	Token            string
	NetworkInterface string
	SSH              SSHConfig

	// 仅运行时字段（不持久化）
	RegToken       string
	Name           string
	ReportInterval float64
	VerifyInterval float64
	ForceRegister  bool
	ConfigPath     string
}

// DefaultReportInterval 是默认的上报间隔（秒）。
const DefaultReportInterval = 3.0

// DefaultVerifyInterval 是默认的审批轮询间隔（秒）。
const DefaultVerifyInterval = 5.0

// DefaultConfigDir 返回平台相关的默认配置目录。
func DefaultConfigDir() string {
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = "."
		}
		return filepath.Join(appData, "collei-agent")
	default:
		if os.Getuid() == 0 {
			return "/etc/collei-agent"
		}
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg != "" {
			return filepath.Join(xdg, "collei-agent")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "collei-agent")
	}
}

// DefaultConfigPath 返回 agent.yaml 的默认路径。
func DefaultConfigPath() string {
	return filepath.Join(DefaultConfigDir(), "agent.yaml")
}

// IsRegistered 返回 Agent 是否已注册（有 URL 和 Token）。
func (c *AgentConfig) IsRegistered() bool {
	return c.ServerURL != "" && c.Token != ""
}

// ValidateForRegister 检查自动注册所需的参数是否齐全。
func (c *AgentConfig) ValidateForRegister() error {
	if c.ServerURL == "" {
		return fmt.Errorf("missing server_url (--url)")
	}
	if c.RegToken == "" {
		return fmt.Errorf("missing reg_token (--reg-token)")
	}
	return nil
}

// ValidateForPassive 检查被动注册所需的参数是否齐全。
func (c *AgentConfig) ValidateForPassive() error {
	if c.ServerURL == "" {
		return fmt.Errorf("missing server_url (--url)")
	}
	if c.Token == "" {
		return fmt.Errorf("missing token (--token)")
	}
	return nil
}

// Save 将持久化配置字段写入 YAML 文件。
func (c *AgentConfig) Save(path string) error {
	if path == "" {
		path = c.ConfigPath
	}
	if path == "" {
		path = DefaultConfigPath()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	p := &persistedConfig{
		ServerURL:        c.ServerURL,
		UUID:             c.UUID,
		Token:            c.Token,
		NetworkInterface: c.NetworkInterface,
	}
	if c.SSH.Enabled {
		p.SSH = &c.SSH
	}

	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	slog.Info("config saved", "path", path)
	return nil
}

// Load 从 YAML 文件读取 Agent 配置。
// 如果文件不存在，返回默认配置。
func Load(path string) *AgentConfig {
	if path == "" {
		path = DefaultConfigPath()
	}

	cfg := &AgentConfig{
		SSH:            SSHConfig{Port: 22},
		ReportInterval: DefaultReportInterval,
		VerifyInterval: DefaultVerifyInterval,
		ConfigPath:     path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("config file not found, using defaults", "path", path)
		} else {
			slog.Warn("failed to read config file", "path", path, "error", err)
		}
		return cfg
	}

	var p persistedConfig
	if err := yaml.Unmarshal(data, &p); err != nil {
		slog.Warn("failed to parse config file", "path", path, "error", err)
		return cfg
	}

	cfg.ServerURL = p.ServerURL
	cfg.UUID = p.UUID
	cfg.Token = p.Token
	cfg.NetworkInterface = p.NetworkInterface
	if p.SSH != nil {
		cfg.SSH = *p.SSH
	}

	slog.Info("config loaded", "path", path)
	return cfg
}
