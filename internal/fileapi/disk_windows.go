//go:build windows

package fileapi

import "golang.org/x/sys/windows"

func getDriveSize(root string) int64 {
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(rootPtr, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0
	}
	return int64(totalBytes)
}
