//go:build !windows

package fileapi

func getDriveSize(_ string) int64 {
	return 0
}

func isWindowsHiddenOrSystem(_ string) bool {
	return false
}
