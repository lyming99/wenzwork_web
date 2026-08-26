//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func taskLogFileHasSingleLink(file *os.File) bool {
	if file == nil {
		return false
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return false
	}
	return info.NumberOfLinks == 1 && info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

func taskLogDiskFreeBytes(path string) (uint64, error) {
	path, err := existingTaskLogProbePath(path)
	if err != nil {
		return 0, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, nil, nil); err != nil {
		return 0, err
	}
	return available, nil
}

// Windows does not expose a portable directory fsync equivalent. The file is
// flushed before Rename and NTFS/ReFS make the same-volume name operation
// atomic; reparse-point checks are repeated whenever the file is reopened.
func syncTaskLogDirectory(string) error { return nil }
