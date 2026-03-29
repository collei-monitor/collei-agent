//go:build windows

package fileapi

import (
	"syscall"

	"golang.org/x/sys/windows"
)

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

func isWindowsHiddenOrSystem(path string) bool {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return false
	}
	return attrs&(syscall.FILE_ATTRIBUTE_HIDDEN|syscall.FILE_ATTRIBUTE_SYSTEM) != 0
}
