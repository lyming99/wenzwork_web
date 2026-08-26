//go:build windows

package main

import (
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/Kodecable/crosspty"
)

const windowsPTYDesktopHelperEnvironment = "WENZWORK_TEST_PTY_DESKTOP_WINDOW"

func TestWindowsSupervisedPTYDesktopIsStable(t *testing.T) {
	first, err := windowsSupervisedPTYDesktopName()
	if err != nil {
		t.Fatal(err)
	}
	second, err := windowsSupervisedPTYDesktopName()
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second != first || windowsSupervisedPTYDesktop.handle == 0 {
		t.Fatalf("hidden desktop = %q/%q handle=%#x", first, second, windowsSupervisedPTYDesktop.handle)
	}
}

func TestWindowsSupervisedPTYPreservesOutputOnHiddenDesktop(t *testing.T) {
	pty, err := startSupervisedPTY(windowsPTYTestConfig([]string{
		"cmd.exe", "/d", "/c", "echo WENZWORK_HIDDEN_CONPTY_OK",
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()
	output, readErr := io.ReadAll(pty)
	exitCode := pty.Wait()
	if readErr != nil {
		t.Fatalf("read hidden ConPTY output: %v", readErr)
	}
	if exitCode != 0 || !strings.Contains(string(output), "WENZWORK_HIDDEN_CONPTY_OK") {
		t.Fatalf("hidden ConPTY output=%q exitCode=%d", output, exitCode)
	}
}

func TestWindowsSupervisedPTYKeepsChildWindowOffInputDesktop(t *testing.T) {
	if os.Getenv(windowsPTYDesktopHelperEnvironment) == "1" {
		title, _ := syscall.UTF16PtrFromString("WenzWork private terminal desktop test")
		message, _ := syscall.UTF16PtrFromString("probe")
		windowsUser32DLL.NewProc("MessageBoxW").Call(
			0,
			uintptr(unsafe.Pointer(message)),
			uintptr(unsafe.Pointer(title)),
			0,
		)
		os.Exit(0)
	}

	config := windowsPTYTestConfig([]string{os.Args[0], "-test.run=^TestWindowsSupervisedPTYKeepsChildWindowOffInputDesktop$"})
	config.Env = append(os.Environ(), windowsPTYDesktopHelperEnvironment+"=1")
	pty, err := startSupervisedPTY(config)
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()

	pid := uint32(pty.Pid())
	deadline := time.Now().Add(5 * time.Second)
	for !windowsDesktopHasVisibleWindow(windowsSupervisedPTYDesktop.handle, pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !windowsDesktopHasVisibleWindow(windowsSupervisedPTYDesktop.handle, pid) {
		t.Fatalf("helper process %d did not create its probe window on the private desktop", pid)
	}
	if windowsInputDesktopHasVisibleWindow(pid) {
		t.Fatalf("helper process %d exposed a visible window on the input desktop", pid)
	}
}

func windowsPTYTestConfig(arguments []string) crosspty.CommandConfig {
	return crosspty.CommandConfig{
		Argv: arguments,
		Env:  os.Environ(),
		Size: crosspty.TermSize{Rows: 24, Cols: 80},
		CloseConfig: crosspty.CloseConfig{
			CloseTimeout: 4 * time.Second,
			KillDelay:    2 * time.Second,
			KillExitCode: 137,
			KillMode:     crosspty.KillModeKillGroupOnSubProcessExit,
		},
	}
}

func windowsDesktopHasVisibleWindow(desktop uintptr, pid uint32) bool {
	if desktop == 0 || pid == 0 {
		return false
	}
	return windowsEnumerateVisibleWindow(
		func(callback uintptr) uintptr {
			result, _, _ := windowsUser32DLL.NewProc("EnumDesktopWindows").Call(desktop, callback, 0)
			return result
		},
		pid,
	)
}

func windowsInputDesktopHasVisibleWindow(pid uint32) bool {
	if pid == 0 {
		return false
	}
	return windowsEnumerateVisibleWindow(
		func(callback uintptr) uintptr {
			result, _, _ := windowsUser32DLL.NewProc("EnumWindows").Call(callback, 0)
			return result
		},
		pid,
	)
}

func windowsEnumerateVisibleWindow(enumerate func(uintptr) uintptr, pid uint32) bool {
	visible := false
	getWindowProcess := windowsUser32DLL.NewProc("GetWindowThreadProcessId")
	isWindowVisible := windowsUser32DLL.NewProc("IsWindowVisible")
	callback := syscall.NewCallback(func(window, _ uintptr) uintptr {
		var windowPID uint32
		getWindowProcess.Call(window, uintptr(unsafe.Pointer(&windowPID)))
		if windowPID == pid {
			shown, _, _ := isWindowVisible.Call(window)
			if shown != 0 {
				visible = true
				return 0
			}
		}
		return 1
	})
	_ = enumerate(callback)
	return visible
}
