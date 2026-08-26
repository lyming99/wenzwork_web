//go:build windows

package main

import (
	"bytes"
	"os"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

func TestReviewedProcessEnvironmentPreservesProtectedPATHEXT(t *testing.T) {
	pathExtensions, found := os.LookupEnv("PATHEXT")
	if !found || pathExtensions == "" {
		t.Skip("PATHEXT is unavailable")
	}
	environment, err := reviewedProcessEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(environment, "PATHEXT="+pathExtensions) {
		t.Fatalf("reviewed environment does not preserve PATHEXT: %#v", environment)
	}
	if _, err := reviewedProcessEnvironment([]string{"PATHEXT=.EXE"}); err != errRPCInvalid {
		t.Fatalf("PATHEXT override error = %v, want %v", err, errRPCInvalid)
	}
}

func TestProcessSupervisorRealWindowsProcessTreeCleanup(t *testing.T) {
	if !platformInteractiveTerminalRuntimeAvailable() {
		t.Skip("Windows ConPTY is unavailable")
	}
	powershell, err := resolveSupervisedExecutable("powershell")
	if err != nil {
		t.Skip("Windows PowerShell is unavailable")
	}
	root := t.TempDir()
	supervisor := newProcessSupervisor()
	t.Cleanup(func() { _ = supervisor.Close() })
	script := `$child = Start-Process -FilePath (Join-Path $env:SystemRoot 'System32\PING.EXE') -ArgumentList '-t','127.0.0.1' -WindowStyle Hidden -PassThru; [Console]::Out.WriteLine('CHILD_PID=' + $child.Id); [Console]::Out.Flush(); Start-Sleep -Seconds 300`
	process, err := supervisor.Start(processLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root,
		Argv: []string{powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script},
		Rows: 24, Columns: 80,
		Limits: processResourceLimits{
			MaximumLifetime: time.Minute, MaximumMemoryBytes: 512 << 20, MaximumOutputBytes: 1 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	childPID := readWindowsChildPID(t, process)
	parentPID := process.Pid()
	if !windowsProcessActive(parentPID) || !windowsProcessActive(childPID) {
		t.Fatalf("process tree was not running before cleanup: parent=%d child=%d", parentPID, childPID)
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() bool {
		return !windowsProcessActive(parentPID) && !windowsProcessActive(childPID)
	})
	if reason := process.reason(); reason != "agent_exit" {
		t.Fatalf("process tree close reason = %q", reason)
	}
}

func readWindowsChildPID(t *testing.T, process *supervisedProcess) int {
	t.Helper()
	result := make(chan int, 1)
	go func() {
		var output bytes.Buffer
		buffer := make([]byte, 1024)
		pattern := regexp.MustCompile(`CHILD_PID=(\d+)`)
		for output.Len() < 64<<10 {
			n, err := process.Read(buffer)
			if n > 0 {
				_, _ = output.Write(buffer[:n])
				match := pattern.FindSubmatch(output.Bytes())
				if len(match) == 2 {
					pid, parseErr := strconv.Atoi(string(match[1]))
					if parseErr == nil && pid > 0 {
						result <- pid
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case pid := <-result:
		return pid
	case <-time.After(10 * time.Second):
		_ = process.Close("test_timeout")
		t.Fatal("real Windows child process did not report its PID")
		return 0
	}
}

func windowsProcessActive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var exitCode uint32
	return windows.GetExitCodeProcess(handle, &exitCode) == nil && exitCode == 259
}
