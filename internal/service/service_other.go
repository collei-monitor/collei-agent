//go:build !windows

package service

import (
	"context"
	"fmt"
)

// IsWindowsService 在非 Windows 平台上始终返回 false
func IsWindowsService() bool { return false }

// RunAsService 在非 Windows 平台上不支持
func RunAsService(_ func(ctx context.Context)) error {
	return fmt.Errorf("windows service not supported on this platform")
}

// Install 在非 Windows 平台上不支持
func Install(_ string) error {
	return fmt.Errorf("windows service not supported on this platform")
}

// Uninstall 在非 Windows 平台上不支持
func Uninstall() error {
	return fmt.Errorf("windows service not supported on this platform")
}

// Start 在非 Windows 平台上不支持
func Start() error {
	return fmt.Errorf("windows service not supported on this platform")
}

// Stop 在非 Windows 平台上不支持
func Stop() error {
	return fmt.Errorf("windows service not supported on this platform")
}
