//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

const (
	windowsDesktopReadObjects  = 0x0001
	windowsDesktopCreateWindow = 0x0002
	windowsDesktopWriteObjects = 0x0080
)

var (
	windowsUser32DLL            = windows.NewLazySystemDLL("user32.dll")
	windowsCreateDesktopW       = windowsUser32DLL.NewProc("CreateDesktopW")
	windowsGetThreadDesktop     = windowsUser32DLL.NewProc("GetThreadDesktop")
	windowsSetThreadDesktop     = windowsUser32DLL.NewProc("SetThreadDesktop")
	windowsSupervisedPTYDesktop windowsPTYDesktopState
)

type windowsPTYDesktopState struct {
	once   sync.Once
	name   string
	handle uintptr
	err    error
}

type windowsPTYDesktopResult struct {
	name   string
	handle uintptr
	err    error
}

// windowsSupervisedPTYDesktopName returns a process-lifetime private desktop.
// Interactive shells are explicitly bound to this desktop through
// STARTUPINFO.lpDesktop, so neither the shell nor any console/GUI descendant
// can flash a window on the signed-in user's input desktop.
func windowsSupervisedPTYDesktopName() (string, error) {
	windowsSupervisedPTYDesktop.once.Do(func() {
		result := <-createWindowsSupervisedPTYDesktop()
		windowsSupervisedPTYDesktop.name = result.name
		windowsSupervisedPTYDesktop.handle = result.handle
		windowsSupervisedPTYDesktop.err = result.err
	})
	return windowsSupervisedPTYDesktop.name, windowsSupervisedPTYDesktop.err
}

func createWindowsSupervisedPTYDesktop() <-chan windowsPTYDesktopResult {
	result := make(chan windowsPTYDesktopResult, 1)
	go func() {
		runtime.LockOSThread()
		threadID := windows.GetCurrentThreadId()
		original, _, callErr := windowsGetThreadDesktop.Call(uintptr(threadID))
		if original == 0 {
			runtime.UnlockOSThread()
			result <- windowsPTYDesktopResult{err: windowsDesktopError("GetThreadDesktop", callErr)}
			return
		}

		name := fmt.Sprintf("WenzWorkTerminal-%d-%s", os.Getpid(), uuid.NewString())
		encodedName, err := syscall.UTF16PtrFromString(name)
		if err != nil {
			runtime.UnlockOSThread()
			result <- windowsPTYDesktopResult{err: fmt.Errorf("encode hidden terminal desktop name: %w", err)}
			return
		}
		access := uintptr(windowsDesktopReadObjects | windowsDesktopCreateWindow | windowsDesktopWriteObjects)
		desktop, _, callErr := windowsCreateDesktopW.Call(
			uintptr(unsafe.Pointer(encodedName)), 0, 0, 0, access, 0,
		)
		runtime.KeepAlive(encodedName)
		if desktop == 0 {
			runtime.UnlockOSThread()
			result <- windowsPTYDesktopResult{err: windowsDesktopError("CreateDesktopW", callErr)}
			return
		}

		// CreateDesktopW assigns the new desktop to its calling thread. Restore
		// the original before returning this OS thread to the Go scheduler. If
		// restoration ever fails, leave the goroutine locked: Go then terminates
		// that OS thread instead of reusing it on the private desktop.
		if restored, _, restoreErr := windowsSetThreadDesktop.Call(original); restored == 0 {
			result <- windowsPTYDesktopResult{
				handle: desktop,
				err:    windowsDesktopError("restore thread desktop", restoreErr),
			}
			return
		}
		runtime.UnlockOSThread()
		result <- windowsPTYDesktopResult{name: name, handle: desktop}
	}()
	return result
}

func windowsDesktopError(operation string, callErr error) error {
	if callErr != nil {
		if errno, ok := callErr.(syscall.Errno); ok && errno == 0 {
			return fmt.Errorf("%s failed", operation)
		}
		return fmt.Errorf("%s: %w", operation, callErr)
	}
	return fmt.Errorf("%s failed", operation)
}
