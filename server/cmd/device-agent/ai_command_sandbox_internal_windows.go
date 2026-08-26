//go:build windows

package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	aiWindowsSandboxGrantMask            = windows.ACCESS_MASK(0x00110156)
	aiWindowsFileAllAccess               = windows.ACCESS_MASK(0x001F01FF)
	aiWindowsDisableMaxPrivilege         = 0x1
	aiWindowsLUAToken                    = 0x4
	aiWindowsWriteRestricted             = 0x8
	aiWindowsJobObjectLimitJobTime       = 0x00000004
	aiWindowsJobObjectLimitActiveProcess = 0x00000008
)

var (
	aiWindowsAdvapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	aiWindowsCreateRestrictedTokenProc = aiWindowsAdvapi32.NewProc("CreateRestrictedToken")
	aiWindowsSetEntriesInACLProc       = aiWindowsAdvapi32.NewProc("SetEntriesInAclW")
)

type aiWindowsSandboxOptions struct {
	workspace        string
	workingDirectory string
	mode             string
	cpuSeconds       uint64
	memoryBytes      uint64
	maxProcesses     uint32
	command          []string
}

type aiWindowsTokenDefaultDACL struct {
	DefaultDACL *windows.ACL
}

func runAICommandSandboxInternal(arguments []string) (bool, int) {
	if len(arguments) == 0 || arguments[0] != aiCommandSandboxWindowsACLInternal {
		return false, 0
	}
	options, err := parseAIWindowsSandboxOptions(arguments[1:])
	if err != nil {
		return true, failAIWindowsSandbox(err)
	}
	exitCode, err := runAIWindowsSandbox(options)
	if err != nil {
		return true, failAIWindowsSandbox(err)
	}
	return true, exitCode
}

func parseAIWindowsSandboxOptions(arguments []string) (aiWindowsSandboxOptions, error) {
	if len(arguments) == 1 && arguments[0] == "--probe" {
		workingDirectory, err := os.Getwd()
		if err != nil {
			return aiWindowsSandboxOptions{}, err
		}
		return aiWindowsSandboxOptions{
			workspace: workingDirectory, workingDirectory: workingDirectory, mode: aiWorkspaceModeReadOnly,
			cpuSeconds: 30, memoryBytes: 256 << 20, maxProcesses: 8,
			command: []string{"cmd.exe", "/d", "/c", "exit", "0"},
		}, nil
	}
	options := aiWindowsSandboxOptions{cpuSeconds: 60, memoryBytes: 512 << 20, maxProcesses: 16}
	for index := 0; index < len(arguments); {
		token := arguments[index]
		index++
		if token == "--" {
			options.command = append([]string(nil), arguments[index:]...)
			break
		}
		if index >= len(arguments) {
			return aiWindowsSandboxOptions{}, fmt.Errorf("missing value after %s", token)
		}
		value := arguments[index]
		index++
		switch token {
		case "--workspace":
			options.workspace = value
		case "--working-directory":
			options.workingDirectory = value
		case "--mode":
			options.mode = value
		case "--cpu-seconds":
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err != nil || parsed == 0 || parsed > 86400 {
				return aiWindowsSandboxOptions{}, errors.New("invalid --cpu-seconds")
			}
			options.cpuSeconds = parsed
		case "--memory-bytes":
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err != nil || parsed < 16<<20 {
				return aiWindowsSandboxOptions{}, errors.New("invalid --memory-bytes")
			}
			options.memoryBytes = parsed
		case "--max-processes":
			parsed, err := strconv.ParseUint(value, 10, 32)
			if err != nil || parsed == 0 || parsed > 4096 {
				return aiWindowsSandboxOptions{}, errors.New("invalid --max-processes")
			}
			options.maxProcesses = uint32(parsed)
		default:
			return aiWindowsSandboxOptions{}, fmt.Errorf("unknown argument %s", token)
		}
	}
	if options.workspace == "" || options.workingDirectory == "" ||
		(options.mode != aiWorkspaceModeReadOnly && options.mode != aiWorkspaceModeWorkspaceWrite) ||
		len(options.command) == 0 {
		return aiWindowsSandboxOptions{}, errors.New("missing workspace, working directory, mode, or command")
	}
	for _, argument := range options.command {
		if argument == "" || strings.IndexByte(argument, 0) >= 0 {
			return aiWindowsSandboxOptions{}, errors.New("invalid command argument")
		}
	}
	return options, nil
}

func runAIWindowsSandbox(options aiWindowsSandboxOptions) (int, error) {
	workspace, err := canonicalAIWindowsSandboxDirectory(options.workspace)
	if err != nil {
		return 0, fmt.Errorf("workspace: %w", err)
	}
	workingDirectory, err := canonicalAIWindowsSandboxDirectory(options.workingDirectory)
	if err != nil {
		return 0, fmt.Errorf("working directory: %w", err)
	}
	if !pathWithinRoot(workspace, workingDirectory) {
		return 0, errors.New("working directory must be inside the workspace")
	}
	options.workspace, options.workingDirectory = workspace, workingDirectory
	workspaceWrite := options.mode == aiWorkspaceModeWorkspaceWrite

	var workspaceSID, tempSID *windows.SID
	if workspaceWrite {
		workspaceSID, err = windows.StringToSid(aiWindowsWorkspaceSID(workspace))
		if err != nil {
			return 0, fmt.Errorf("parse workspace capability SID: %w", err)
		}
		if err := editAIWindowsSandboxACL(workspace, workspaceSID, windows.GRANT_ACCESS, true); err != nil {
			return 0, fmt.Errorf("grant workspace capability: %w", err)
		}
	}

	privateTemp := ""
	if workspaceWrite {
		temporaryRoot, err := canonicalAIWindowsSandboxDirectory(os.TempDir())
		if err != nil {
			return 0, fmt.Errorf("temporary root: %w", err)
		}
		if pathWithinRoot(workspace, temporaryRoot) {
			return 0, errors.New("Windows ACL temp root must be outside the workspace")
		}
		privateTemp, err = os.MkdirTemp(temporaryRoot, "wenzwork-sandbox-")
		if err != nil {
			return 0, fmt.Errorf("create private temp: %w", err)
		}
		defer func() { _ = os.RemoveAll(privateTemp) }()
		privateTemp, err = canonicalAIWindowsSandboxDirectory(privateTemp)
		if err != nil {
			return 0, fmt.Errorf("private temp: %w", err)
		}
		if pathWithinRoot(workspace, privateTemp) || pathWithinRoot(privateTemp, workspace) {
			return 0, errors.New("private temp overlaps the workspace")
		}
		tempSID, err = windows.StringToSid(aiWindowsTempSID(privateTemp))
		if err != nil {
			return 0, fmt.Errorf("parse temp capability SID: %w", err)
		}
		if err := editAIWindowsSandboxACL(privateTemp, tempSID, windows.GRANT_ACCESS, false); err != nil {
			return 0, fmt.Errorf("grant temp capability: %w", err)
		}
		defer func() {
			if cleanupErr := editAIWindowsSandboxACL(privateTemp, tempSID, windows.REVOKE_ACCESS, false); cleanupErr != nil {
				fmt.Fprintf(os.Stderr, "windows-acl-run: cleanup: revoke temp capability: %v\n", cleanupErr)
			}
		}()
		oldTMP, hadTMP := os.LookupEnv("TMP")
		oldTEMP, hadTEMP := os.LookupEnv("TEMP")
		defer func() {
			if hadTMP {
				_ = os.Setenv("TMP", oldTMP)
			} else {
				_ = os.Unsetenv("TMP")
			}
			if hadTEMP {
				_ = os.Setenv("TEMP", oldTEMP)
			} else {
				_ = os.Unsetenv("TEMP")
			}
		}()
		if err := os.Setenv("TMP", privateTemp); err != nil {
			return 0, fmt.Errorf("set TMP: %w", err)
		}
		if err := os.Setenv("TEMP", privateTemp); err != nil {
			return 0, fmt.Errorf("set TEMP: %w", err)
		}
	}

	var current windows.Token
	if err := windows.OpenProcessToken(
		windows.CurrentProcess(),
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_ASSIGN_PRIMARY,
		&current,
	); err != nil {
		return 0, fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer current.Close()
	restricted, err := createAIWindowsRestrictedToken(current, workspaceWrite, workspaceSID, tempSID)
	if err != nil {
		return 0, err
	}
	defer restricted.Close()
	exitCode, err := spawnAIWindowsRestrictedProcess(restricted, options)
	if err != nil {
		return 0, err
	}
	return int(exitCode), nil
}

func canonicalAIWindowsSandboxDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func aiWindowsWorkspaceSID(workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	first := (uint32(digest[0])|uint32(digest[1])<<8|uint32(digest[2])<<16|uint32(digest[3])<<24)%(1<<30-1) + 1
	second := (uint32(digest[4])|uint32(digest[5])<<8|uint32(digest[6])<<16|uint32(digest[7])<<24)%(1<<30-1) + 1
	return fmt.Sprintf("S-1-4-%d-%d", first, second)
}

func aiWindowsTempSID(privateTemp string) string {
	digest := sha256.Sum256(append([]byte("temp\x00"), []byte(privateTemp)...))
	first := (uint32(digest[0])|uint32(digest[1])<<8|uint32(digest[2])<<16|uint32(digest[3])<<24)%(1<<30-1) + 1
	second := (uint32(digest[4])|uint32(digest[5])<<8|uint32(digest[6])<<16|uint32(digest[7])<<24)%(1<<30-1) + 1
	return fmt.Sprintf("S-1-4-%d-%d-1", first, second)
}

func editAIWindowsSandboxACL(path string, sid *windows.SID, mode windows.ACCESS_MODE, skipExact bool) error {
	unlock, err := lockAIWindowsSandboxACL(path)
	if err != nil {
		return err
	}
	defer unlock()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		if err == nil {
			err = errors.New("security descriptor is nil")
		}
		return err
	}
	oldACL, _, err := descriptor.DACL()
	if err != nil && !errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return err
	}
	if skipExact && hasExactAIWindowsSandboxGrant(oldACL, sid) {
		return nil
	}
	var pinner runtime.Pinner
	pinner.Pin(sid)
	defer pinner.Unpin()
	permissions := aiWindowsSandboxGrantMask
	if mode == windows.REVOKE_ACCESS {
		permissions = 0
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: permissions,
		AccessMode:        mode,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	newACL, err := setEntriesAIWindowsSandboxACL(&entry, oldACL)
	if err != nil {
		return err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(newACL)))
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, newACL, nil); err != nil {
		return err
	}
	runtime.KeepAlive(descriptor)
	return nil
}

func setEntriesAIWindowsSandboxACL(entry *windows.EXPLICIT_ACCESS, oldACL *windows.ACL) (*windows.ACL, error) {
	var newACL *windows.ACL
	status, _, _ := aiWindowsSetEntriesInACLProc.Call(
		1,
		uintptr(unsafe.Pointer(entry)),
		uintptr(unsafe.Pointer(oldACL)),
		uintptr(unsafe.Pointer(&newACL)),
	)
	if status != 0 {
		return nil, syscall.Errno(status)
	}
	if newACL == nil {
		return nil, errors.New("SetEntriesInAclW returned a nil ACL")
	}
	return newACL, nil
}

func hasExactAIWindowsSandboxGrant(acl *windows.ACL, sid *windows.SID) bool {
	if acl == nil {
		return false
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil || ace == nil {
			return false
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE ||
			ace.Mask != aiWindowsSandboxGrantMask {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if windows.EqualSid(aceSID, sid) {
			return true
		}
	}
	return false
}

func lockAIWindowsSandboxACL(path string) (func(), error) {
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(path))))
	name := fmt.Sprintf("Local\\WenzWorkSandboxAcl-%x", digest[:8])
	namePointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	mutex, createErr := windows.CreateMutex(nil, false, namePointer)
	if mutex == 0 || createErr != nil && !errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
		return nil, createErr
	}
	wait, err := windows.WaitForSingleObject(mutex, windows.INFINITE)
	if err != nil || wait != windows.WAIT_OBJECT_0 && wait != windows.WAIT_ABANDONED {
		windows.CloseHandle(mutex)
		if err == nil {
			err = fmt.Errorf("unexpected mutex wait result %d", wait)
		}
		return nil, err
	}
	return func() {
		_ = windows.ReleaseMutex(mutex)
		_ = windows.CloseHandle(mutex)
	}, nil
}

func createAIWindowsRestrictedToken(
	current windows.Token,
	workspaceWrite bool,
	workspaceSID, tempSID *windows.SID,
) (windows.Token, error) {
	groups, err := current.GetTokenGroups()
	if err != nil {
		return 0, fmt.Errorf("GetTokenGroups: %w", err)
	}
	var logonSID *windows.SID
	for _, group := range groups.AllGroups() {
		if group.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID {
			logonSID = group.Sid
			break
		}
	}
	if logonSID == nil {
		return 0, errors.New("current token has no logon SID")
	}
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return 0, fmt.Errorf("CreateWellKnownSid: %w", err)
	}
	restricting := []windows.SIDAndAttributes{{Sid: logonSID}, {Sid: worldSID}}
	if workspaceWrite {
		if workspaceSID == nil || tempSID == nil {
			return 0, errors.New("workspace-write requires workspace and temp capability SIDs")
		}
		restricting = append(restricting, windows.SIDAndAttributes{Sid: workspaceSID}, windows.SIDAndAttributes{Sid: tempSID})
	}
	var pinner runtime.Pinner
	for _, item := range restricting {
		pinner.Pin(item.Sid)
	}
	defer pinner.Unpin()
	var restricted windows.Token
	created, _, callErr := aiWindowsCreateRestrictedTokenProc.Call(
		uintptr(current),
		aiWindowsDisableMaxPrivilege|aiWindowsLUAToken|aiWindowsWriteRestricted,
		0, 0,
		0, 0,
		uintptr(len(restricting)), uintptr(unsafe.Pointer(&restricting[0])),
		uintptr(unsafe.Pointer(&restricted)),
	)
	if created == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", callErr)
	}
	defaultGrant := worldSID
	if workspaceWrite {
		defaultGrant = tempSID
	}
	if err := setAIWindowsTokenDefaultDACL(restricted, defaultGrant); err != nil {
		restricted.Close()
		return 0, err
	}
	runtime.KeepAlive(groups)
	return restricted, nil
}

func setAIWindowsTokenDefaultDACL(token windows.Token, grantSID *windows.SID) error {
	var required uint32
	err := windows.GetTokenInformation(token, windows.TokenDefaultDacl, nil, 0, &required)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || required == 0 {
		return fmt.Errorf("GetTokenInformation TokenDefaultDacl size: %w", err)
	}
	buffer := make([]byte, required)
	if err := windows.GetTokenInformation(token, windows.TokenDefaultDacl, &buffer[0], uint32(len(buffer)), &required); err != nil {
		return fmt.Errorf("GetTokenInformation TokenDefaultDacl: %w", err)
	}
	current := (*aiWindowsTokenDefaultDACL)(unsafe.Pointer(&buffer[0]))
	if current.DefaultDACL == nil {
		return errors.New("restricted token carries no default DACL")
	}
	var pinner runtime.Pinner
	pinner.Pin(grantSID)
	defer pinner.Unpin()
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: aiWindowsFileAllAccess,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(grantSID),
		},
	}
	merged, err := setEntriesAIWindowsSandboxACL(&entry, current.DefaultDACL)
	if err != nil {
		return fmt.Errorf("merge token default DACL: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(merged)))
	replacement := aiWindowsTokenDefaultDACL{DefaultDACL: merged}
	if err := windows.SetTokenInformation(
		token, windows.TokenDefaultDacl,
		(*byte)(unsafe.Pointer(&replacement)), uint32(unsafe.Sizeof(replacement)),
	); err != nil {
		return fmt.Errorf("SetTokenInformation TokenDefaultDacl: %w", err)
	}
	runtime.KeepAlive(buffer)
	return nil
}

func spawnAIWindowsRestrictedProcess(token windows.Token, options aiWindowsSandboxOptions) (uint32, error) {
	stdin, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return 0, fmt.Errorf("GetStdHandle stdin: %w", err)
	}
	stdout, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return 0, fmt.Errorf("GetStdHandle stdout: %w", err)
	}
	stderr, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		return 0, fmt.Errorf("GetStdHandle stderr: %w", err)
	}
	for _, handle := range []windows.Handle{stdin, stdout, stderr} {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return 0, fmt.Errorf("SetHandleInformation: %w", err)
		}
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.PerJobUserTimeLimit = int64(options.cpuSeconds * 10_000_000)
	limits.BasicLimitInformation.LimitFlags =
		windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
			aiWindowsJobObjectLimitJobTime |
			aiWindowsJobObjectLimitActiveProcess |
			windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY |
			windows.JOB_OBJECT_LIMIT_JOB_MEMORY
	limits.BasicLimitInformation.ActiveProcessLimit = options.maxProcesses
	limits.ProcessMemoryLimit = uintptr(options.memoryBytes)
	limits.JobMemoryLimit = uintptr(options.memoryBytes)
	if _, err := windows.SetInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)),
	); err != nil {
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	quoted := make([]string, len(options.command))
	for index, argument := range options.command {
		quoted[index] = syscall.EscapeArg(argument)
	}
	commandLine, err := windows.UTF16FromString(strings.Join(quoted, " "))
	if err != nil {
		return 0, err
	}
	workingDirectory, err := windows.UTF16PtrFromString(options.workingDirectory)
	if err != nil {
		return 0, err
	}
	startup := windows.StartupInfo{
		Cb:       uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Flags:    windows.STARTF_USESTDHANDLES,
		StdInput: stdin, StdOutput: stdout, StdErr: stderr,
	}
	configureBackgroundStartupInfo(&startup)
	process := windows.ProcessInformation{}
	if err := windows.CreateProcessAsUser(
		token, nil, &commandLine[0], nil, nil, true,
		windowsBackgroundCreationFlags(windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT),
		nil, workingDirectory, &startup, &process,
	); err != nil {
		return 0, fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	defer windows.CloseHandle(process.Process)
	defer windows.CloseHandle(process.Thread)
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		_ = windows.TerminateProcess(process.Process, aiCommandSandboxRunnerFailureExit)
		return 0, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		_ = windows.TerminateProcess(process.Process, aiCommandSandboxRunnerFailureExit)
		return 0, fmt.Errorf("ResumeThread: %w", err)
	}
	if _, err := windows.WaitForSingleObject(process.Process, windows.INFINITE); err != nil {
		return 0, fmt.Errorf("WaitForSingleObject child: %w", err)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.Process, &exitCode); err != nil {
		return 0, fmt.Errorf("GetExitCodeProcess: %w", err)
	}
	return exitCode, nil
}

func failAIWindowsSandbox(err error) int {
	fmt.Fprintf(os.Stderr, "windows-acl-run: %v\n", err)
	return aiCommandSandboxRunnerFailureExit
}
