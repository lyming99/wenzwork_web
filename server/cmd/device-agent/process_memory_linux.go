//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func platformProcessTreeMemoryBytes(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, errProcessMemoryUnavailable
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	children := make(map[int][]int)
	rss := make(map[int]uint64)
	for _, entry := range entries {
		processID, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || processID <= 0 {
			continue
		}
		contents, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if readErr != nil {
			continue
		}
		line := string(contents)
		closing := strings.LastIndex(line, ") ")
		if closing < 0 {
			continue
		}
		fields := strings.Fields(line[closing+2:])
		// fields starts at proc(5) field 3; ppid is 4 and RSS pages is 24.
		if len(fields) <= 21 {
			continue
		}
		parentID, parentErr := strconv.Atoi(fields[1])
		pages, pagesErr := strconv.ParseUint(fields[21], 10, 64)
		if parentErr != nil || pagesErr != nil {
			continue
		}
		children[parentID] = append(children[parentID], processID)
		rss[processID] = pages * uint64(os.Getpagesize())
	}
	if _, found := rss[pid]; !found {
		return 0, errProcessMemoryUnavailable
	}
	processes := []int{pid}
	var total uint64
	for index := 0; index < len(processes); index++ {
		processID := processes[index]
		total += rss[processID]
		processes = append(processes, children[processID]...)
	}
	return total, nil
}
