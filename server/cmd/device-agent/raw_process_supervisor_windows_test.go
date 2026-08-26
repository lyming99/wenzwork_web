//go:build windows

package main

import (
	"bytes"
	"io"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRawProcessSupervisorRealWindowsProcessTreeCleanup(t *testing.T) {
	powershell, err := resolveSupervisedExecutable("powershell")
	if err != nil {
		t.Skip("Windows PowerShell is unavailable")
	}
	root := t.TempDir()
	supervisor := newRawProcessSupervisor()
	t.Cleanup(func() { _ = supervisor.Close() })
	// Configure the test child explicitly because this is a raw pipe rather
	// than the production task wrapper. It lets the test parse its own ASCII
	// PID sentinel while exercising the actual Job Object implementation.
	script := `$utf8 = [System.Text.UTF8Encoding]::new($false); [Console]::OutputEncoding = $utf8; $OutputEncoding = $utf8; ` +
		`$child = Start-Process -FilePath (Join-Path $env:SystemRoot 'System32\PING.EXE') -ArgumentList '-t','127.0.0.1' -WindowStyle Hidden -PassThru; ` +
		`[Console]::Out.WriteLine('CHILD_PID=' + $child.Id); [Console]::Error.WriteLine('raw-stderr'); [Console]::Out.Flush(); Start-Sleep -Seconds 300`
	process, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root,
		Argv: []string{powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script},
		Limits: processResourceLimits{
			MaximumLifetime: time.Minute, MaximumMemoryBytes: 512 << 20, MaximumOutputBytes: 1 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, process.Stderr()) }()
	childPID := readRawWindowsChildPID(t, process)
	parentPID := process.Pid()
	if !windowsProcessActive(parentPID) || !windowsProcessActive(childPID) {
		t.Fatalf("raw process tree was not running before cleanup: parent=%d child=%d", parentPID, childPID)
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	// Windows can report a clean PowerShell exit after a Job Object is closed;
	// process liveness below is the reliable tree-recovery assertion.
	_ = process.Wait()
	eventually(t, 10*time.Second, func() bool {
		return !windowsProcessActive(parentPID) && !windowsProcessActive(childPID)
	})
	if reason := process.reason(); reason != "agent_exit" {
		t.Fatalf("raw process tree close reason = %q", reason)
	}
}

func TestCurrentWindowsProcessIdentityCanBeInspected(t *testing.T) {
	if _, err := currentWindowsProcessIsLocalSystem(); err != nil {
		t.Fatalf("inspect current Windows process identity: %v", err)
	}
}

func readRawWindowsChildPID(t *testing.T, process *rawSupervisedProcess) int {
	t.Helper()
	result := make(chan int, 1)
	go func() {
		var output bytes.Buffer
		buffer := make([]byte, 1024)
		pattern := regexp.MustCompile(`CHILD_PID=(\d+)`)
		for output.Len() < 64<<10 {
			n, err := process.Stdout().Read(buffer)
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
		t.Fatal("raw Windows child process did not report its PID")
		return 0
	}
}
