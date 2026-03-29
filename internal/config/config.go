package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/net/idna"
	"gopkg.in/yaml.v3"
)

// SSHConfig 存储 SSH 隧道配置。
type SSHConfig struct {
	Enabled      bool `yaml:"enabled"`
	Port         int  `yaml:"port"`
	CaConfigured bool `yaml:"ca_configured"`
}

// TerminalConfig 存储终端直连（ConPTY）配置。
type TerminalConfig struct {
	Enabled      bool   `yaml:"enabled"`
	DefaultShell string `yaml:"default_shell,omitempty"`
}

// FileAPIConfig 存储文件 API 配置。
type FileAPIConfig struct {
	Enabled bool `yaml:"enabled"`
}

// persistedConfig 是写入 agent.yaml 的字段子集。
type persistedConfig struct {
	ServerURL        string          `yaml:"server_url,omitempty"`
	UUID             string          `yaml:"uuid,omitempty"`
	Token            string          `yaml:"token,omitempty"`
	NetworkInterface string          `yaml:"network_interface,omitempty"`
	SSH              *SSHConfig      `yaml:"ssh,omitempty"`
	Terminal         *TerminalConfig `yaml:"terminal,omitempty"`
	FileAPI          *FileAPIConfig  `yaml:"file_api,omitempty"`
	CAPublicKeyPath  string          `yaml:"ca_public_key_path,omitempty"`
	AutoUpdate       *bool           `yaml:"auto_update,omitempty"`
}

// AgentConfig 存储完整的 Agent 配置。
type AgentConfig struct {
	// 持久化字段
	ServerURL        string
	UUID             string
	Token            string
	NetworkInterface string
	SSH              SSHConfig
	Terminal         TerminalConfig
	FileAPI          FileAPIConfig
	CAPublicKeyPath  string
	AutoUpdate       bool

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
		// 优先选择 ProgramData 用于服务模式 (SYSTEM 账户)。
		// 回退到 APPDATA 用于交互模式。
		if pd := os.Getenv("ProgramData"); pd != "" {
			serviceDir := filepath.Join(pd, "collei-agent")
			// 如果服务目录已存在，使用它（服务模式）。
			if _, err := os.Stat(serviceDir); err == nil {
				return serviceDir
			}
		}
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

// DefaultCAPublicKeyPath 返回 CA 公钥的默认路径。
func DefaultCAPublicKeyPath() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(DefaultConfigDir(), "ca.pub")
	default:
		return "/etc/ssh/collei-ca.pub"
	}
}

// ServiceConfigDir 返回 Windows 服务模式下的配置目录。
// 非 Windows 平台返回 DefaultConfigDir()。
func ServiceConfigDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "collei-agent")
		}
	}
	return DefaultConfigDir()
}

// IsRegistered 返回 Agent 是否已注册（有 URL 和 Token）。
func (c *AgentConfig) IsRegistered() bool {
	return c.ServerURL != "" && c.Token != ""
}

// NormalizeIDNURL 将 URL 中的国际化域名转换为 Punycode，
// 以支持中文域名等 IDN 地址。
func NormalizeIDNURL(rawURL string) string {
	if rawURL == "" {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}

	host := u.Hostname()
	port := u.Port()

	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		// 已经是纯 ASCII 或无法转换，原样返回
		return rawURL
	}
	if ascii == host {
		return rawURL
	}

	slog.Info("IDN domain normalized", "original", host, "punycode", ascii)

	if port != "" {
		u.Host = ascii + ":" + port
	} else {
		u.Host = ascii
	}
	return u.String()
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
	if c.Terminal.Enabled {
		p.Terminal = &c.Terminal
	}
	if c.FileAPI.Enabled {
		p.FileAPI = &c.FileAPI
	}
	if c.CAPublicKeyPath != "" {
		p.CAPublicKeyPath = c.CAPublicKeyPath
	}
	if !c.AutoUpdate {
		v := false
		p.AutoUpdate = &v
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
		AutoUpdate:     true,
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
	if p.Terminal != nil {
		cfg.Terminal = *p.Terminal
	}
	if p.FileAPI != nil {
		cfg.FileAPI = *p.FileAPI
	}
	if p.CAPublicKeyPath != "" {
		cfg.CAPublicKeyPath = p.CAPublicKeyPath
	}
	if p.AutoUpdate != nil {
		cfg.AutoUpdate = *p.AutoUpdate
	}

	slog.Info("config loaded", "path", path)
	return cfg
}
