package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// DefaultDownloadTimeout 是二进制下载的超时时间。
	DefaultDownloadTimeout = 5 * time.Minute

	// GitHubAPITimeout 是 GitHub API 请求的超时时间。
	GitHubAPITimeout = 15 * time.Second

	// GitHubRepo 是 Agent 的 GitHub 仓库路径。
	GitHubRepo = "collei-monitor/collei-agent"
)

// Updater 封装 Agent 自更新的核心逻辑。
type Updater struct {
	configDir      string
	serverURL      string // 面板地址，用于 proxy fallback
	token          string // agent token，用于 proxy 鉴权
	currentVersion string
}

// NewUpdater 创建一个新的 Updater 实例。
func NewUpdater(configDir, serverURL, token, currentVersion string) *Updater {
	return &Updater{
		configDir:      configDir,
		serverURL:      strings.TrimRight(serverURL, "/"),
		token:          token,
		currentVersion: currentVersion,
	}
}

// ConfigDir 返回配置目录路径。
func (u *Updater) ConfigDir() string {
	return u.configDir
}

// CurrentVersion 返回当前 Agent 版本。
func (u *Updater) CurrentVersion() string {
	return u.currentVersion
}

// ReleaseInfo 描述一个 GitHub Release 的版本信息。
type ReleaseInfo struct {
	Tag         string
	DownloadURL string
}

// NeedsUpdate 比较当前版本和目标版本，判断是否需要更新。
// 当前版本为 "dev" 时跳过自动更新。
func NeedsUpdate(currentVersion, targetVersion string) bool {
	if currentVersion == "dev" || targetVersion == "" {
		return false
	}
	return currentVersion != targetVersion
}

// CheckGitHubLatest 查询 GitHub Releases API 获取最新版本。
// GitHub 不可达时返回错误。
func (u *Updater) CheckGitHubLatest() (*ReleaseInfo, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", GitHubRepo)
	assetName := fmt.Sprintf("collei-agent-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		assetName += ".exe"
	}

	client := &http.Client{Timeout: GitHubAPITimeout}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github api request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github api: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read github response: %w", err)
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("parse github response: %w", err)
	}

	for _, asset := range release.Assets {
		if asset.Name == assetName {
			return &ReleaseInfo{
				Tag:         release.TagName,
				DownloadURL: asset.BrowserDownloadURL,
			}, nil
		}
	}

	return nil, fmt.Errorf("no asset found for %s", assetName)
}

// Upgrade 执行完整的二进制下载和替换流程。
// 不包含状态文件写入和重启（由调用方处理）。
func (u *Updater) Upgrade(downloadURL, checksum string) error {
	currentPath, err := executablePath()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}

	// 下载新二进制（写入当前二进制所在目录，避免 /tmp noexec 问题）
	tmpPath, err := u.download(downloadURL, filepath.Dir(currentPath))
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer func() {
		// 清理临时文件（替换成功后文件已被 rename，Remove 会静默失败）
		os.Remove(tmpPath)
	}()

	// 校验 checksum
	if checksum != "" {
		if err := verifyChecksum(tmpPath, checksum); err != nil {
			return fmt.Errorf("checksum verification: %w", err)
		}
	}

	// 验证新二进制可执行
	if err := verifyBinary(tmpPath); err != nil {
		return fmt.Errorf("binary verification: %w", err)
	}

	// 替换二进制
	if err := replaceBinary(tmpPath, currentPath); err != nil {
		return fmt.Errorf("replace binary: %w", err)
	}

	slog.Info("updater: binary replaced successfully", "path", currentPath)
	return nil
}

// TriggerRestart 触发 Agent 进程重启。
func TriggerRestart() {
	if runtime.GOOS != "windows" && os.Getuid() == 0 {
		if _, err := exec.LookPath("systemctl"); err == nil {
			slog.Info("updater: triggering systemd restart")
			cmd := exec.Command("systemctl", "restart", "collei-agent")
			if err := cmd.Start(); err != nil {
				slog.Error("updater: systemctl restart failed", "error", err)
				return
			}
			// 不等待完成，systemctl 会终止当前进程
			os.Exit(0)
		}
	}

	slog.Warn("updater: cannot auto-restart (not root or no systemd), please restart manually")
}

// --- 内部方法 ---

// download 下载文件到临时路径。优先直连，失败后尝试面板代理。
// dir 指定临时文件所在目录，避免 /tmp noexec 导致后续验证失败。
func (u *Updater) download(downloadURL, dir string) (string, error) {
	tmpFile, err := os.CreateTemp(dir, ".collei-agent-update-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	// 尝试直连下载
	if err := httpDownload(downloadURL, tmpPath); err != nil {
		slog.Warn("updater: direct download failed, trying proxy", "error", err)

		// 尝试面板代理下载
		if u.serverURL == "" || u.token == "" {
			os.Remove(tmpPath)
			return "", fmt.Errorf("direct download failed and proxy not available: %w", err)
		}
		proxyErr := u.proxyDownload(downloadURL, tmpPath)
		if proxyErr != nil {
			os.Remove(tmpPath)
			return "", fmt.Errorf("proxy download also failed: direct=%v, proxy=%v", err, proxyErr)
		}
		slog.Info("updater: downloaded via proxy")
	}

	return tmpPath, nil
}

// httpDownload 通过 HTTP 直连下载文件。
func httpDownload(downloadURL, destPath string) error {
	client := &http.Client{Timeout: DefaultDownloadTimeout}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

// proxyDownload 通过面板代理端点下载文件。
func (u *Updater) proxyDownload(originalURL, destPath string) error {
	proxyURL := fmt.Sprintf("%s/api/v1/agent/download?url=%s",
		u.serverURL, url.QueryEscape(originalURL))

	client := &http.Client{Timeout: DefaultDownloadTimeout}
	req, err := http.NewRequest("GET", proxyURL, nil)
	if err != nil {
		return fmt.Errorf("create proxy request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+u.token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("proxy request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("proxy http %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

// executablePath 返回当前二进制的真实路径（解析符号链接）。
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// verifyChecksum 校验文件的 SHA256 是否与预期一致。
func verifyChecksum(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("sha256 mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

// verifyBinary 设置执行权限并运行新二进制的 version 命令来验证其可执行性。
func verifyBinary(path string) error {
	if err := os.Chmod(path, 0755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	cmd := exec.Command(path, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("execute failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	slog.Debug("updater: new binary verified", "output", strings.TrimSpace(string(out)))
	return nil
}

// replaceBinary 原子替换当前二进制。
func replaceBinary(newPath, currentPath string) error {
	if runtime.GOOS == "windows" {
		// Windows 不允许替换运行中的 exe，先重命名旧文件
		oldPath := currentPath + ".old"
		os.Remove(oldPath) // 清理之前的 .old
		if err := os.Rename(currentPath, oldPath); err != nil {
			return fmt.Errorf("rename old binary: %w", err)
		}
		if err := os.Rename(newPath, currentPath); err != nil {
			// 回滚
			os.Rename(oldPath, currentPath)
			return fmt.Errorf("rename new binary: %w", err)
		}
		os.Remove(oldPath)
	} else {
		// Unix: 可以直接替换运行中的二进制（inode 替换）
		if err := os.Rename(newPath, currentPath); err != nil {
			return fmt.Errorf("rename: %w", err)
		}
	}

	return os.Chmod(currentPath, 0o755)
}
