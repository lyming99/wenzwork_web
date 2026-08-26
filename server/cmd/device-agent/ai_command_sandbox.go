package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

var errAICommandSandboxUnavailable = errors.New("AI command sandbox is unavailable")

const (
	aiCommandSandboxBackendNone       = "none"
	aiCommandSandboxBackendBwrap      = "bubblewrap"
	aiCommandSandboxBackendLandlock   = "landlock"
	aiCommandSandboxBackendSeatbelt   = "seatbelt"
	aiCommandSandboxBackendWindowsACL = "windows-acl"

	aiCommandSandboxEnforcementDisabled = "disabled"
	aiCommandSandboxEnforcementFull     = "full"
	aiCommandSandboxEnforcementPartial  = "partial"

	aiCommandSandboxLandlockInternal   = "__wenzwork_ai_landlock_run"
	aiCommandSandboxWindowsACLInternal = "__wenzwork_ai_windows_acl_run"
	aiCommandSandboxRunnerFailureExit  = 127
)

type aiCommandSandboxRequest struct {
	Mode             string
	WorkspaceRoot    string
	WorkingDirectory string
	Argv             []string
	AllowNetwork     bool
}

type aiCommandSandboxRunnerFailureRule struct {
	AllowedExitCodes   []int
	FatalSignatures    []string
	InformationalLines []string
}

type aiCommandSandboxLaunch struct {
	Argv                 []string
	WorkingDirectory     string
	SandboxMode          string
	Status               string
	NetworkAllowed       bool
	HardNetworkIsolation bool
	Backend              string
	Enforcement          string
	DenialSignatures     []string
	RunnerFailureRules   []aiCommandSandboxRunnerFailureRule
}

func (launch aiCommandSandboxLaunch) runnerFailed(exitCode int, stderr string) bool {
	if exitCode == 0 {
		return false
	}
	lines := strings.Split(strings.ReplaceAll(stderr, "\r\n", "\n"), "\n")
	for _, rule := range launch.RunnerFailureRules {
		if len(rule.AllowedExitCodes) > 0 && !slices.Contains(rule.AllowedExitCodes, exitCode) {
			continue
		}
		for _, line := range lines {
			normalized := strings.ToLower(strings.TrimSpace(line))
			if normalized == "" || containsFoldedExact(rule.InformationalLines, normalized) {
				continue
			}
			for _, signature := range rule.FatalSignatures {
				if strings.Contains(normalized, strings.ToLower(signature)) {
					return true
				}
			}
		}
	}
	return false
}

func containsFoldedExact(values []string, normalized string) bool {
	for _, value := range values {
		if strings.ToLower(strings.TrimSpace(value)) == normalized {
			return true
		}
	}
	return false
}

type aiCommandSandboxPreparer func(aiCommandSandboxRequest) (aiCommandSandboxLaunch, error)

type aiCommandSandboxRuntime struct {
	Bubblewrap          string
	LandlockRunner      string
	LandlockEnforcement string
	Seatbelt            string
	WindowsACLRunner    string
	TemporaryRoot       string
}

var (
	aiCommandSandboxRuntimeOnce  sync.Once
	aiCommandSandboxRuntimeState aiCommandSandboxRuntime
)

func currentAICommandSandboxRuntime() aiCommandSandboxRuntime {
	aiCommandSandboxRuntimeOnce.Do(func() {
		aiCommandSandboxRuntimeState = probeAICommandSandboxRuntime()
	})
	return aiCommandSandboxRuntimeState
}

func prepareAICommandSandbox(request aiCommandSandboxRequest) (aiCommandSandboxLaunch, error) {
	return prepareAICommandSandboxForPlatform(request, currentAICommandSandboxRuntime())
}

func prepareAICommandSandboxForPlatform(
	request aiCommandSandboxRequest,
	runtime aiCommandSandboxRuntime,
) (aiCommandSandboxLaunch, error) {
	root, rootErr := canonicalAICommandSandboxDirectory(request.WorkspaceRoot)
	workingDirectory, workingErr := canonicalAICommandSandboxDirectory(request.WorkingDirectory)
	if !validAIWorkspaceMode(request.Mode) || rootErr != nil || workingErr != nil ||
		!filepath.IsAbs(root) || !filepath.IsAbs(workingDirectory) || !pathWithinRoot(root, workingDirectory) ||
		len(request.Argv) == 0 {
		return aiCommandSandboxLaunch{}, errRPCInvalid
	}
	for _, argument := range request.Argv {
		if argument == "" || strings.IndexByte(argument, 0) >= 0 {
			return aiCommandSandboxLaunch{}, errRPCInvalid
		}
	}

	if request.Mode == aiWorkspaceModeFullAccess {
		return aiCommandSandboxLaunch{
			Argv: append([]string(nil), request.Argv...), WorkingDirectory: workingDirectory,
			SandboxMode:    aiWorkspaceModeFullAccess,
			Status:         "danger-full-access: filesystem and network sandbox disabled; process resource limits remain active",
			NetworkAllowed: true, Backend: aiCommandSandboxBackendNone, Enforcement: aiCommandSandboxEnforcementDisabled,
		}, nil
	}

	if runtime.Bubblewrap != "" {
		arguments := []string{
			runtime.Bubblewrap,
			"--die-with-parent", "--new-session", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
			"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		}
		if !request.AllowNetwork {
			arguments = append(arguments, "--unshare-net")
		}
		status := "bubblewrap: read-only filesystem"
		if request.Mode == aiWorkspaceModeWorkspaceWrite {
			arguments = append(arguments, "--tmpfs", "/tmp", "--bind", root, root)
			status = "bubblewrap: workspace-write filesystem with private /tmp"
		}
		arguments = append(arguments,
			"--chdir", workingDirectory,
			"--setenv", "HOME", root,
			"--setenv", "TMPDIR", "/tmp",
			"--setenv", "TMP", "/tmp",
			"--setenv", "TEMP", "/tmp",
			"--",
		)
		arguments = append(arguments, request.Argv...)
		if request.AllowNetwork {
			status += "; network allowed"
		} else {
			status += "; network namespace disabled"
		}
		return aiCommandSandboxLaunch{
			Argv: arguments, WorkingDirectory: workingDirectory, SandboxMode: request.Mode, Status: status,
			NetworkAllowed: request.AllowNetwork, HardNetworkIsolation: !request.AllowNetwork,
			Backend: aiCommandSandboxBackendBwrap, Enforcement: aiCommandSandboxEnforcementFull,
			DenialSignatures:   []string{"read-only file system"},
			RunnerFailureRules: []aiCommandSandboxRunnerFailureRule{{FatalSignatures: []string{"bwrap: "}}},
		}, nil
	}

	if runtime.LandlockRunner != "" {
		enforcement := runtime.LandlockEnforcement
		if enforcement != aiCommandSandboxEnforcementFull && enforcement != aiCommandSandboxEnforcementPartial {
			return aiCommandSandboxLaunch{}, errAICommandSandboxUnavailable
		}
		arguments := []string{runtime.LandlockRunner, aiCommandSandboxLandlockInternal, "--ro", "/", "--rw", "/dev/null"}
		if request.Mode == aiWorkspaceModeWorkspaceWrite {
			writable := []string{"/tmp"}
			if temporary := strings.TrimSpace(runtime.TemporaryRoot); temporary != "" && temporary != "/tmp" {
				writable = append(writable, temporary)
			}
			writable = append(writable, root)
			for _, path := range writable {
				arguments = append(arguments, "--rw", path)
			}
		}
		arguments = append(arguments, "--")
		arguments = append(arguments, request.Argv...)
		status := fmt.Sprintf("Landlock: %s filesystem (%s enforcement)", request.Mode, enforcement)
		if request.AllowNetwork {
			status += "; network allowed"
		} else {
			status += "; Landlock does not isolate network; the execution gate still rejects unapproved network commands"
		}
		return aiCommandSandboxLaunch{
			Argv: arguments, WorkingDirectory: workingDirectory, SandboxMode: request.Mode, Status: status,
			NetworkAllowed: request.AllowNetwork, HardNetworkIsolation: false,
			Backend: aiCommandSandboxBackendLandlock, Enforcement: enforcement,
			DenialSignatures: []string{"permission denied"},
			RunnerFailureRules: []aiCommandSandboxRunnerFailureRule{{
				AllowedExitCodes: []int{125}, FatalSignatures: []string{"landlock-run: "},
				InformationalLines: []string{"landlock-run: partial enforcement (older Landlock ABI)"},
			}},
		}, nil
	}

	if runtime.Seatbelt != "" {
		profile := []string{
			"(version 1)",
			"(allow default)",
			"(deny file-write*)",
			"(allow file-write* (literal \"/dev/null\"))",
		}
		status := "Seatbelt: read-only filesystem"
		if request.Mode == aiWorkspaceModeWorkspaceWrite {
			writable := []string{root, "/tmp"}
			if temporary := strings.TrimSpace(runtime.TemporaryRoot); temporary != "" {
				writable = append(writable, temporary)
			}
			writable = compactAICommandSandboxPaths(writable)
			for _, path := range writable {
				profile = append(profile, "(allow file-write* (subpath "+strconv.Quote(path)+"))")
			}
			status = "Seatbelt: workspace-write filesystem with temporary directory access"
		}
		if !request.AllowNetwork {
			profile = append(profile, "(deny network*)")
			status += "; network denied"
		} else {
			status += "; network allowed"
		}
		arguments := []string{runtime.Seatbelt, "-p", strings.Join(profile, "\n"), "--"}
		arguments = append(arguments, request.Argv...)
		return aiCommandSandboxLaunch{
			Argv: arguments, WorkingDirectory: workingDirectory, SandboxMode: request.Mode, Status: status,
			NetworkAllowed: request.AllowNetwork, HardNetworkIsolation: !request.AllowNetwork,
			Backend: aiCommandSandboxBackendSeatbelt, Enforcement: aiCommandSandboxEnforcementFull,
			DenialSignatures:   []string{"operation not permitted"},
			RunnerFailureRules: []aiCommandSandboxRunnerFailureRule{{FatalSignatures: []string{"sandbox-exec: "}}},
		}, nil
	}

	if runtime.WindowsACLRunner != "" {
		arguments := []string{
			runtime.WindowsACLRunner, aiCommandSandboxWindowsACLInternal,
			"--workspace", root,
			"--working-directory", workingDirectory,
			"--mode", request.Mode,
			"--cpu-seconds", "60",
			"--memory-bytes", strconv.FormatUint(512<<20, 10),
			"--max-processes", "16",
			"--",
		}
		arguments = append(arguments, request.Argv...)
		status := "Windows WRITE_RESTRICTED token + ACL: partial filesystem enforcement"
		if request.Mode == aiWorkspaceModeWorkspaceWrite {
			status += "; workspace capability SID and private temp are writable"
		} else {
			status += "; no write capability SID is present"
		}
		if request.AllowNetwork {
			status += "; network allowed"
		} else {
			status += "; Windows ACL does not isolate network; the execution gate still rejects unapproved network commands"
		}
		return aiCommandSandboxLaunch{
			Argv: arguments, WorkingDirectory: workingDirectory, SandboxMode: request.Mode, Status: status,
			NetworkAllowed: request.AllowNetwork, HardNetworkIsolation: false,
			Backend: aiCommandSandboxBackendWindowsACL, Enforcement: aiCommandSandboxEnforcementPartial,
			DenialSignatures: []string{"access is denied", "access to the path", "permission denied"},
			RunnerFailureRules: []aiCommandSandboxRunnerFailureRule{{
				AllowedExitCodes: []int{aiCommandSandboxRunnerFailureExit}, FatalSignatures: []string{"windows-acl-run: "},
			}},
		}, nil
	}

	return aiCommandSandboxLaunch{}, errAICommandSandboxUnavailable
}

func canonicalAICommandSandboxDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("sandbox path is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func compactAICommandSandboxPaths(paths []string) []string {
	result := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func aiCommandSandboxRuntimeAvailable() bool {
	runtime := currentAICommandSandboxRuntime()
	return runtime.Bubblewrap != "" || runtime.LandlockRunner != "" || runtime.Seatbelt != "" || runtime.WindowsACLRunner != ""
}
