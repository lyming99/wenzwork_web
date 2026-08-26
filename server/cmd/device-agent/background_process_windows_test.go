//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestConfigureBackgroundProcessHidesConsoleWindow(t *testing.T) {
	command := exec.Command("cmd.exe")
	configureBackgroundProcess(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow {
		t.Fatalf("background process attributes = %#v, want HideWindow=true", command.SysProcAttr)
	}
	if command.SysProcAttr.CreationFlags&windowsCreateNoWindow == 0 {
		t.Fatalf("background process flags = %#x, want CREATE_NO_WINDOW", command.SysProcAttr.CreationFlags)
	}
}

func TestConfigureBackgroundProcessPreservesExistingAttributes(t *testing.T) {
	const existingFlag = 0x00000200
	command := exec.Command("cmd.exe")
	command.SysProcAttr = &syscall.SysProcAttr{CmdLine: "reviewed command line", CreationFlags: existingFlag}
	configureBackgroundProcess(command)
	if command.SysProcAttr.CmdLine != "reviewed command line" {
		t.Fatalf("background process command line = %q", command.SysProcAttr.CmdLine)
	}
	if command.SysProcAttr.CreationFlags&existingFlag == 0 || command.SysProcAttr.CreationFlags&windowsCreateNoWindow == 0 {
		t.Fatalf("background process flags = %#x, want existing and CREATE_NO_WINDOW", command.SysProcAttr.CreationFlags)
	}
}

func TestWindowsBackgroundNativeProcessSettingsPreserveExistingFlags(t *testing.T) {
	const existingCreationFlag = uint32(0x00000200)
	creationFlags := windowsBackgroundCreationFlags(existingCreationFlag)
	if creationFlags&existingCreationFlag == 0 || creationFlags&windowsCreateNoWindow == 0 {
		t.Fatalf("native background process flags = %#x, want existing and CREATE_NO_WINDOW", creationFlags)
	}

	const existingStartupFlag = uint32(0x00000100)
	startup := windows.StartupInfo{Flags: existingStartupFlag, ShowWindow: syscall.SW_SHOW}
	configureBackgroundStartupInfo(&startup)
	if startup.Flags&existingStartupFlag == 0 || startup.Flags&windows.STARTF_USESHOWWINDOW == 0 {
		t.Fatalf("native background startup flags = %#x, want existing and STARTF_USESHOWWINDOW", startup.Flags)
	}
	if startup.ShowWindow != syscall.SW_HIDE {
		t.Fatalf("native background ShowWindow = %d, want SW_HIDE", startup.ShowWindow)
	}
}
