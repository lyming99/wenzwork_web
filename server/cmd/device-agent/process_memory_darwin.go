//go:build darwin

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

func platformProcessTreeMemoryBytes(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, errProcessMemoryUnavailable
	}
	command := exec.Command("/bin/ps", "-axo", "pid=,ppid=,rss=")
	command.Env = minimalTerminalEnvironment()
	contents, err := command.Output()
	if err != nil || len(contents) > 4<<20 {
		return 0, errProcessMemoryUnavailable
	}
	children := make(map[int][]int)
	rss := make(map[int]uint64)
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		processID, idErr := strconv.Atoi(fields[0])
		parentID, parentErr := strconv.Atoi(fields[1])
		kilobytes, memoryErr := strconv.ParseUint(fields[2], 10, 64)
		if idErr != nil || parentErr != nil || memoryErr != nil || processID <= 0 {
			continue
		}
		children[parentID] = append(children[parentID], processID)
		rss[processID] = kilobytes << 10
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
