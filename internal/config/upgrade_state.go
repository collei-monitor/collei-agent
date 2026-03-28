package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// UpgradeState 记录正在进行的升级操作状态，用于进程重启后恢复。
type UpgradeState struct {
	ExecutionID     string `json:"execution_id,omitempty"` // 面板任务 ID（自动更新时为空）
	TargetVersion   string `json:"target_version"`
	PreviousVersion string `json:"previous_version"`
	StartedAt       int64  `json:"started_at"`
}

const upgradeStateFile = "upgrade_state.json"

// UpgradeStatePath 返回升级状态文件的完整路径。
func UpgradeStatePath(configDir string) string {
	return filepath.Join(configDir, upgradeStateFile)
}

// ReadUpgradeState 读取升级状态文件。文件不存在时返回 nil, nil。
func ReadUpgradeState(configDir string) (*UpgradeState, error) {
	path := UpgradeStatePath(configDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read upgrade state: %w", err)
	}

	var state UpgradeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse upgrade state: %w", err)
	}
	return &state, nil
}

// WriteUpgradeState 原子写入升级状态文件（先写临时文件再 rename）。
func WriteUpgradeState(configDir string, state *UpgradeState) error {
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal upgrade state: %w", err)
	}

	tmpPath := UpgradeStatePath(configDir) + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write upgrade state tmp: %w", err)
	}

	if err := os.Rename(tmpPath, UpgradeStatePath(configDir)); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename upgrade state: %w", err)
	}
	return nil
}

// RemoveUpgradeState 删除升级状态文件。文件不存在时不报错。
func RemoveUpgradeState(configDir string) error {
	err := os.Remove(UpgradeStatePath(configDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove upgrade state: %w", err)
	}
	return nil
}
