//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsInteractiveCommandContext struct {
	token       windows.Token
	environment []string
}

func openWindowsInteractiveCommandContext() (windowsInteractiveCommandContext, error) {
	var lastErr error
	for _, sessionID := range activeWindowsSessionIDs() {
		var token windows.Token
		if err := windows.WTSQueryUserToken(sessionID, &token); err != nil {
			lastErr = err
			continue
		}
		environment, err := token.Environ(false)
		if err != nil {
			_ = token.Close()
			lastErr = err
			continue
		}
		filtered := filterWindowsInteractiveCommandEnvironment(environment)
		if len(filtered) == 0 {
			_ = token.Close()
			lastErr = errors.New("interactive Windows environment is empty")
			continue
		}
		return windowsInteractiveCommandContext{token: token, environment: filtered}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no interactive Windows session")
	}
	return windowsInteractiveCommandContext{}, lastErr
}

func activeWindowsSessionIDs() []uint32 {
	const invalidSessionID = ^uint32(0)
	result := make([]uint32, 0, 2)
	seen := make(map[uint32]struct{})
	appendSession := func(sessionID uint32) {
		if sessionID == invalidSessionID {
			return
		}
		if _, exists := seen[sessionID]; exists {
			return
		}
		seen[sessionID] = struct{}{}
		result = append(result, sessionID)
	}
	// Prefer the physical console, then support RDP-only deployments where no
	// console session exists. This avoids silently falling back to LocalSystem
	// just because the user is connected through Remote Desktop.
	appendSession(windows.WTSGetActiveConsoleSessionId())
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil || sessions == nil {
		return result
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	for _, session := range unsafe.Slice(sessions, int(count)) {
		if session.State == windows.WTSActive {
			appendSession(session.SessionID)
		}
	}
	return result
}

func (context windowsInteractiveCommandContext) Close() {
	if context.token != 0 {
		_ = context.token.Close()
	}
}

func filterWindowsInteractiveCommandEnvironment(values []string) []string {
	return filterCommandRuntimeEnvironment(values)
}

func windowsInteractiveCommandEnvironment() []string {
	context, err := openWindowsInteractiveCommandContext()
	if err != nil {
		return nil
	}
	defer context.Close()
	return append([]string(nil), context.environment...)
}

func configureRawProcessCommand(command *exec.Cmd) (func(), error) {
	if command == nil {
		return nil, errors.New("raw command is nil")
	}
	// In a normal console (including development), WTSQueryUserToken can be
	// unavailable. Preserve the established current-process behavior there.
	// The installed LocalSystem service can query the active user's token.
	context, err := openWindowsInteractiveCommandContext()
	if err != nil {
		localSystem, tokenErr := currentWindowsProcessIsLocalSystem()
		if tokenErr != nil {
			return nil, fmt.Errorf("%w: cannot verify the Windows service identity: %v", errTaskExecutionContextUnavailable, tokenErr)
		}
		if localSystem {
			return nil, fmt.Errorf("%w: no signed-in Windows user", errTaskExecutionContextUnavailable)
		}
		// A foreground development process already runs as the intended user.
		// Rebuild the same closed runtime allowlist from that process so npm
		// shims still find Node and the CLI keeps its per-user login state.
		command.Env = mergeInteractiveCommandEnvironment(filterCommandRuntimeEnvironment(os.Environ()), command.Env)
		return func() {}, nil
	}
	command.Env = mergeInteractiveCommandEnvironment(context.environment, command.Env)
	attributes := syscall.SysProcAttr{Token: syscall.Token(context.token)}
	if command.SysProcAttr != nil {
		attributes = *command.SysProcAttr
		attributes.Token = syscall.Token(context.token)
	}
	command.SysProcAttr = &attributes
	return context.Close, nil
}

func currentWindowsProcessIsLocalSystem() (bool, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return false, err
	}
	if user == nil || user.User.Sid == nil {
		return false, errors.New("current Windows token has no user SID")
	}
	return user.User.Sid.IsWellKnown(windows.WinLocalSystemSid), nil
}

func attachRawProcessTree(command *exec.Cmd) (rawProcessTree, error) {
	if command == nil || command.Process == nil || command.Process.Pid < 1 {
		return nil, errors.New("raw process is not running")
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	cleanup := func(cause error) (rawProcessTree, error) {
		_ = windows.CloseHandle(job)
		return nil, cause
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		return cleanup(err)
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(command.Process.Pid),
	)
	if err != nil {
		return cleanup(err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return cleanup(fmt.Errorf("assign process to Job Object: %w", err))
	}
	return &windowsRawProcessTree{job: job}, nil
}

type windowsRawProcessTree struct {
	job  windows.Handle
	once sync.Once
	err  error
}

func (tree *windowsRawProcessTree) Close() error {
	if tree == nil || tree.job == 0 {
		return nil
	}
	tree.once.Do(func() {
		// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE guarantees that this closes every
		// descendant as well as the direct child without constructing taskkill.
		tree.err = windows.CloseHandle(tree.job)
	})
	return tree.err
}
