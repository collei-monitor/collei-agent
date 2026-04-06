package config

import (
	"os"
	"strings"
)

// IsContainer 检测当前进程是否运行在容器环境中。
// 通过检查 Docker/Podman 的标志文件和 cgroup 信息判断。
func IsContainer() bool {
	// Docker 创建的标志文件
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	// Podman 创建的标志文件
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}

	// 检查 cgroup（兜底方案）
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	content := strings.ToLower(string(data))
	return strings.Contains(content, "docker") ||
		strings.Contains(content, "containerd") ||
		strings.Contains(content, "podman") ||
		strings.Contains(content, "lxc")
}
