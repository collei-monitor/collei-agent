//go:build !windows

package fileapi

func getDriveSize(_ string) int64 {
	return 0
}
