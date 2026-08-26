//go:build windows

package main

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcessMemoryCounters struct {
	Size                       uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var getProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

func platformProcessTreeMemoryBytes(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, errProcessMemoryUnavailable
	}
	processes, err := windowsProcessTree(uint32(pid))
	if err != nil {
		return 0, err
	}
	var total uint64
	measuredRoot := false
	for _, processID := range processes {
		memory, measureErr := windowsProcessMemoryBytes(processID)
		if measureErr != nil {
			if processID == uint32(pid) {
				return 0, measureErr
			}
			continue
		}
		if processID == uint32(pid) {
			measuredRoot = true
		}
		total += memory
	}
	if !measuredRoot {
		return 0, errProcessMemoryUnavailable
	}
	return total, nil
}

func windowsProcessTree(root uint32) ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	children := make(map[uint32][]uint32)
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
		entry.Size = uint32(unsafe.Sizeof(windows.ProcessEntry32{}))
		err = windows.Process32Next(snapshot, &entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	result := []uint32{root}
	for index := 0; index < len(result); index++ {
		result = append(result, children[result[index]]...)
	}
	return result, nil
}

func windowsProcessMemoryBytes(pid uint32) (uint64, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	counters := windowsProcessMemoryCounters{Size: uint32(unsafe.Sizeof(windowsProcessMemoryCounters{}))}
	result, _, callErr := getProcessMemoryInfo.Call(
		uintptr(handle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size),
	)
	if result == 0 {
		if callErr != nil && callErr != windows.ERROR_SUCCESS {
			return 0, callErr
		}
		return 0, errProcessMemoryUnavailable
	}
	return uint64(counters.WorkingSetSize), nil
}
