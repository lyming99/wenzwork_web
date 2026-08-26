//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// CREATE_NO_WINDOW prevents console-subsystem background commands from
// allocating a visible console. Keep this centralized so task runners,
// probes, MCP servers, and constrained RPC commands all use the same policy.
const windowsCreateNoWindow = 0x08000000

func configureBackgroundProcess(command *exec.Cmd) {
	if command == nil {
		return
	}
	attributes := syscall.SysProcAttr{}
	if command.SysProcAttr != nil {
		attributes = *command.SysProcAttr
	}
	attributes.HideWindow = true
	attributes.CreationFlags = windowsBackgroundCreationFlags(attributes.CreationFlags)
	command.SysProcAttr = &attributes
}

// windowsBackgroundCreationFlags is also used by native CreateProcess paths
// which do not go through os/exec. Keeping the flag policy here prevents a
// newly added probe or sandbox runner from silently regressing to a visible
// console window.
func windowsBackgroundCreationFlags(flags uint32) uint32 {
	return flags | windowsCreateNoWindow
}

func configureBackgroundStartupInfo(startup *windows.StartupInfo) {
	if startup == nil {
		return
	}
	startup.Flags |= windows.STARTF_USESHOWWINDOW
	startup.ShowWindow = syscall.SW_HIDE
}
