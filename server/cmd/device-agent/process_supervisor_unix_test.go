//go:build !windows

package main

import (
	"bytes"
	"errors"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProcessSupervisorRealUnixProcessTreeCleanup(t *testing.T) {
	if !platformInteractiveTerminalRuntimeAvailable() {
		t.Skip("Unix PTY is unavailable")
	}
	shell, err := resolveSupervisedExecutable("sh")
	if err != nil {
		t.Skip("POSIX shell is unavailable")
	}
	root := t.TempDir()
	supervisor := newProcessSupervisor()
	t.Cleanup(func() { _ = supervisor.Close() })
	process, err := supervisor.Start(processLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root,
		Argv: []string{shell, "-c", `sleep 300 & child=$!; printf 'CHILD_PID=%s\n' "$child"; wait`},
		Rows: 24, Columns: 80,
		Limits: processResourceLimits{
			MaximumLifetime: time.Minute, MaximumMemoryBytes: 512 << 20, MaximumOutputBytes: 1 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	childPID := readUnixChildPID(t, process)
	parentPID := process.Pid()
	if !unixProcessActive(parentPID) || !unixProcessActive(childPID) {
		t.Fatalf("process tree was not running before cleanup: parent=%d child=%d", parentPID, childPID)
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() bool {
		return !unixProcessActive(parentPID) && !unixProcessActive(childPID)
	})
	if reason := process.reason(); reason != "agent_exit" {
		t.Fatalf("process tree close reason = %q", reason)
	}
}

func readUnixChildPID(t *testing.T, process *supervisedProcess) int {
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
		t.Fatal("real Unix child process did not report its PID")
		return 0
	}
}

func unixProcessActive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "linux" {
		status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err == nil {
			// A test binary used as container PID 1 is not an init/subreaper,
			// so a killed grandchild can remain as a zombie until the container
			// exits. It is no longer executable work and must not be mistaken
			// for the orphan process this test is intended to detect.
			if closeParen := bytes.LastIndexByte(status, ')'); closeParen >= 0 {
				fields := bytes.Fields(status[closeParen+1:])
				if len(fields) > 0 && bytes.Equal(fields[0], []byte("Z")) {
					return false
				}
			}
		}
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
