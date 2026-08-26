package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPrepareAICommandSandboxLinuxReadOnlyHasNoWritableWorkspace(t *testing.T) {
	root := t.TempDir()
	launch, err := prepareAICommandSandboxForPlatform(aiCommandSandboxRequest{
		Mode: aiWorkspaceModeReadOnly, WorkspaceRoot: root, WorkingDirectory: root,
		Argv: []string{"/bin/sh", "-c", "echo safe"}, AllowNetwork: false,
	}, aiCommandSandboxRuntime{Bubblewrap: "/usr/bin/bwrap", TemporaryRoot: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if launch.SandboxMode != aiWorkspaceModeReadOnly || launch.NetworkAllowed || !launch.HardNetworkIsolation ||
		!containsAICommandArgumentSequence(launch.Argv, "--ro-bind", "/", "/") ||
		!slices.Contains(launch.Argv, "--unshare-net") || slices.Contains(launch.Argv, "--tmpfs") ||
		containsAICommandArgumentSequence(launch.Argv, "--bind", root, root) {
		t.Fatalf("read-only launch = %#v", launch)
	}
}

func TestPrepareAICommandSandboxLinuxWorkspaceWriteBindsOnlyWorkspaceAndPrivateTemp(t *testing.T) {
	root := t.TempDir()
	workingDirectory := filepath.Join(root, "subdir")
	if err := os.MkdirAll(workingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	launch, err := prepareAICommandSandboxForPlatform(aiCommandSandboxRequest{
		Mode: aiWorkspaceModeWorkspaceWrite, WorkspaceRoot: root, WorkingDirectory: workingDirectory,
		Argv: []string{"/bin/sh", "-c", "echo safe"}, AllowNetwork: false,
	}, aiCommandSandboxRuntime{Bubblewrap: "/usr/bin/bwrap", TemporaryRoot: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if launch.SandboxMode != aiWorkspaceModeWorkspaceWrite || launch.NetworkAllowed || !launch.HardNetworkIsolation ||
		!containsAICommandArgumentSequence(launch.Argv, "--bind", root, root) ||
		!containsAICommandArgumentSequence(launch.Argv, "--tmpfs", "/tmp") ||
		!containsAICommandArgumentSequence(launch.Argv, "--chdir", workingDirectory) {
		t.Fatalf("workspace-write launch = %#v", launch)
	}
}

func TestPrepareAICommandSandboxFullAccessIsExplicitPassthrough(t *testing.T) {
	root := t.TempDir()
	argv := []string{"shell", "-c", "echo unrestricted"}
	launch, err := prepareAICommandSandboxForPlatform(aiCommandSandboxRequest{
		Mode: aiWorkspaceModeFullAccess, WorkspaceRoot: root, WorkingDirectory: root, Argv: argv,
	}, aiCommandSandboxRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(launch.Argv, argv) || launch.SandboxMode != aiWorkspaceModeFullAccess ||
		!launch.NetworkAllowed || launch.HardNetworkIsolation || !strings.Contains(launch.Status, "sandbox disabled") {
		t.Fatalf("full-access launch = %#v", launch)
	}
}

func TestPrepareAICommandSandboxLinuxFallsBackToLandlock(t *testing.T) {
	root := t.TempDir()
	launch, err := prepareAICommandSandboxForPlatform(aiCommandSandboxRequest{
		Mode: aiWorkspaceModeWorkspaceWrite, WorkspaceRoot: root, WorkingDirectory: root,
		Argv: []string{"/bin/sh", "-c", "echo safe"}, AllowNetwork: false,
	}, aiCommandSandboxRuntime{
		LandlockRunner: "device-agent", LandlockEnforcement: aiCommandSandboxEnforcementPartial, TemporaryRoot: "/tmp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if launch.Backend != aiCommandSandboxBackendLandlock || launch.Enforcement != aiCommandSandboxEnforcementPartial ||
		launch.HardNetworkIsolation || !containsAICommandArgumentSequence(launch.Argv, "--ro", "/") ||
		!containsAICommandArgumentSequence(launch.Argv, "--rw", root) || launch.Argv[1] != aiCommandSandboxLandlockInternal {
		t.Fatalf("landlock launch = %#v", launch)
	}
}

func TestPrepareAICommandSandboxWindowsUsesRestrictedTokenACLRunner(t *testing.T) {
	root := t.TempDir()
	for _, mode := range []string{aiWorkspaceModeReadOnly, aiWorkspaceModeWorkspaceWrite} {
		launch, err := prepareAICommandSandboxForPlatform(aiCommandSandboxRequest{
			Mode: mode, WorkspaceRoot: root, WorkingDirectory: root, Argv: []string{"powershell", "-Command", "echo safe"},
		}, aiCommandSandboxRuntime{WindowsACLRunner: `C:\Program Files\WenzWork\device-agent.exe`})
		if err != nil {
			t.Fatalf("mode %s error = %v", mode, err)
		}
		if launch.Backend != aiCommandSandboxBackendWindowsACL || launch.Enforcement != aiCommandSandboxEnforcementPartial ||
			launch.HardNetworkIsolation || launch.Argv[1] != aiCommandSandboxWindowsACLInternal ||
			!containsAICommandArgumentSequence(launch.Argv, "--mode", mode) {
			t.Fatalf("mode %s launch = %#v", mode, launch)
		}
	}
}

func TestPrepareAICommandSandboxConfinedModesFailClosedWithoutRunner(t *testing.T) {
	root := t.TempDir()
	_, err := prepareAICommandSandboxForPlatform(aiCommandSandboxRequest{
		Mode: aiWorkspaceModeReadOnly, WorkspaceRoot: root, WorkingDirectory: root,
		Argv: []string{"shell", "-c", "echo safe"},
	}, aiCommandSandboxRuntime{})
	if !errors.Is(err, errAICommandSandboxUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestAICommandSandboxRunnerFailureRequiresExitAndSignature(t *testing.T) {
	launch := aiCommandSandboxLaunch{RunnerFailureRules: []aiCommandSandboxRunnerFailureRule{{
		AllowedExitCodes: []int{127}, FatalSignatures: []string{"windows-acl-run: "},
	}}}
	if !launch.runnerFailed(127, "windows-acl-run: token failed") ||
		launch.runnerFailed(1, "windows-acl-run: token failed") ||
		launch.runnerFailed(127, "ordinary child failure") {
		t.Fatal("runner failure classification did not require both pieces of evidence")
	}
}

func containsAICommandArgumentSequence(arguments []string, sequence ...string) bool {
	if len(sequence) == 0 || len(sequence) > len(arguments) {
		return false
	}
	for index := 0; index <= len(arguments)-len(sequence); index++ {
		if slices.Equal(arguments[index:index+len(sequence)], sequence) {
			return true
		}
	}
	return false
}
