//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func taskExecCommand(executable string, arguments []string) (*exec.Cmd, error) {
	switch strings.ToLower(filepath.Ext(executable)) {
	case ".cmd", ".bat":
		comspec := os.Getenv("COMSPEC")
		if comspec == "" {
			comspec = filepath.Join(os.Getenv("SYSTEMROOT"), "System32", "cmd.exe")
		}
		if !filepath.IsAbs(comspec) || strings.ContainsAny(comspec, "\x00\r\n\"") {
			return nil, errors.New("COMSPEC is invalid")
		}
		line, argumentEnvironment, err := windowsBatchCommandLine(executable, arguments)
		if err != nil {
			return nil, err
		}
		command := exec.Command(comspec)
		command.Env = replaceWindowsTaskExecEnvironment(os.Environ(), argumentEnvironment)
		command.SysProcAttr = &syscall.SysProcAttr{
			// npm-style batch shims inherit cmd.exe's active code page.  Set it
			// before launching the reviewed command, then call the shim so cmd
			// returns the shim's real exit code. Output remains raw bytes.
			CmdLine: `"` + comspec + `" /d /v:off /s /c "` + windowsCmdUTF8Bootstrap + " & call " + line + `"`,
		}
		configureBackgroundProcess(command)
		return command, nil
	case ".ps1":
		powershell, err := resolveSupervisedExecutable("powershell.exe")
		if err != nil {
			return nil, err
		}
		script, argumentEnvironment, err := windowsPowerShellTaskCommand(executable, arguments)
		if err != nil {
			return nil, err
		}
		command := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
		command.Env = replaceWindowsTaskExecEnvironment(os.Environ(), argumentEnvironment)
		configureBackgroundProcess(command)
		return command, nil
	default:
		command := exec.Command(executable, arguments...)
		configureBackgroundProcess(command)
		return command, nil
	}
}

func windowsPowerShellTaskCommand(executable string, arguments []string) (string, []string, error) {
	values := append([]string{executable}, arguments...)
	if len(values) > 512 {
		return "", nil, errors.New("too many PowerShell arguments")
	}
	environment := make([]string, 0, len(values))
	references := make([]string, 0, len(values))
	totalBytes := 0
	for index, value := range values {
		if strings.ContainsAny(value, "\x00\r\n") || len(value) > 8<<10 {
			return "", nil, errors.New("PowerShell argument is invalid")
		}
		totalBytes += len(value)
		if totalBytes > 24<<10 {
			return "", nil, errors.New("PowerShell arguments are too large")
		}
		name := fmt.Sprintf("WENZWORK_TASK_EXEC_ARG_%03d", index)
		environment = append(environment, name+"="+value)
		references = append(references, "$env:"+name)
	}
	if len(references) == 0 {
		return "", nil, errors.New("PowerShell executable is missing")
	}
	// Pull every value from the private environment rather than interpolating
	// it into -Command. This keeps spaces, quotes and metacharacters as argv
	// data while still allowing the UTF-8 bootstrap and a real exit code.
	return windowsPowerShellUTF8Bootstrap + "; $global:LASTEXITCODE = $null; & " + references[0] + " @(" + strings.Join(references[1:], ",") +
		"); $__wenzworkSuccess = $?; $__wenzworkExit = $global:LASTEXITCODE; " +
		"if (-not $__wenzworkSuccess) { if ($null -ne $__wenzworkExit -and $__wenzworkExit -ne 0) { exit [int]$__wenzworkExit }; exit 1 }; exit 0", environment, nil
}

func windowsBatchCommandLine(executable string, arguments []string) (string, []string, error) {
	values := append([]string{executable}, arguments...)
	if len(values) > 512 {
		return "", nil, errors.New("too many batch arguments")
	}
	quoted := make([]string, 0, len(values))
	environment := make([]string, 0, len(values))
	totalBytes := 0
	for index, value := range values {
		if strings.ContainsAny(value, "\x00\r\n\"") || len(value) > 8<<10 {
			return "", nil, errors.New("batch argument is invalid")
		}
		totalBytes += len(value)
		if totalBytes > 24<<10 {
			return "", nil, errors.New("batch arguments are too large")
		}
		name := fmt.Sprintf("WENZWORK_TASK_EXEC_ARG_%03d", index)
		quoted = append(quoted, `"%`+name+`%"`)
		environment = append(environment, name+"="+value)
	}
	return strings.Join(quoted, " "), environment, nil
}

func replaceWindowsTaskExecEnvironment(base, replacements []string) []string {
	names := make(map[string]struct{}, len(replacements))
	for _, variable := range replacements {
		name, _, _ := strings.Cut(variable, "=")
		names[strings.ToUpper(name)] = struct{}{}
	}
	result := make([]string, 0, len(base)+len(replacements))
	for _, variable := range base {
		name, _, found := strings.Cut(variable, "=")
		if !found {
			continue
		}
		if _, replaced := names[strings.ToUpper(name)]; !replaced {
			result = append(result, variable)
		}
	}
	return append(result, replacements...)
}
