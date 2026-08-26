//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	aiLandlockCreateRulesetVersion = 1 << 0
	aiLandlockRulePathBeneath      = 1
	aiLandlockMaximumABI           = 5
	aiLandlockRunnerFailureExit    = 125

	aiLandlockFSExecute    = uint64(1) << 0
	aiLandlockFSWriteFile  = uint64(1) << 1
	aiLandlockFSReadFile   = uint64(1) << 2
	aiLandlockFSReadDir    = uint64(1) << 3
	aiLandlockFSRemoveDir  = uint64(1) << 4
	aiLandlockFSRemoveFile = uint64(1) << 5
	aiLandlockFSMakeChar   = uint64(1) << 6
	aiLandlockFSMakeDir    = uint64(1) << 7
	aiLandlockFSMakeReg    = uint64(1) << 8
	aiLandlockFSMakeSock   = uint64(1) << 9
	aiLandlockFSMakeFIFO   = uint64(1) << 10
	aiLandlockFSMakeBlock  = uint64(1) << 11
	aiLandlockFSMakeSym    = uint64(1) << 12
	aiLandlockFSRefer      = uint64(1) << 13
	aiLandlockFSTruncate   = uint64(1) << 14
	aiLandlockFSIoctlDev   = uint64(1) << 15
)

type aiLandlockRulesetAttr struct {
	HandledAccessFS uint64
}

type aiLandlockPathBeneathAttr struct {
	AllowedAccess uint64
	ParentFD      int32
}

type aiLandlockCLI struct {
	probe     bool
	readOnly  []string
	readWrite []string
	command   []string
}

func runAICommandSandboxInternal(arguments []string) (bool, int) {
	if len(arguments) == 0 || arguments[0] != aiCommandSandboxLandlockInternal {
		return false, 0
	}
	return true, runAICommandLandlock(arguments[1:])
}

func runAICommandLandlock(arguments []string) int {
	parsed, err := parseAICommandLandlock(arguments)
	if err != nil {
		return failAICommandLandlock("usage error", err)
	}
	if parsed.probe {
		partial, err := restrictAICommandLandlock([]string{"/"}, nil)
		if err != nil {
			return failAICommandLandlock("landlock ruleset error", err)
		}
		if partial {
			fmt.Fprintln(os.Stdout, "landlock: partially enforced (older ABI)")
		} else {
			fmt.Fprintln(os.Stdout, "landlock: fully enforced")
		}
		return 0
	}
	resolved, err := exec.LookPath(parsed.command[0])
	if err != nil {
		return failAICommandLandlock("exec lookup failed", err)
	}
	partial, err := restrictAICommandLandlock(parsed.readOnly, parsed.readWrite)
	if err != nil {
		return failAICommandLandlock("landlock ruleset error", err)
	}
	if partial {
		fmt.Fprintln(os.Stderr, "landlock-run: partial enforcement (older Landlock ABI)")
	}
	if err := unix.Exec(resolved, parsed.command, os.Environ()); err != nil {
		return failAICommandLandlock("exec failed", err)
	}
	return 0
}

func parseAICommandLandlock(arguments []string) (aiLandlockCLI, error) {
	if len(arguments) == 1 && arguments[0] == "--probe" {
		return aiLandlockCLI{probe: true}, nil
	}
	var parsed aiLandlockCLI
	for index := 0; index < len(arguments); {
		token := arguments[index]
		index++
		if token == "--" {
			parsed.command = append([]string(nil), arguments[index:]...)
			break
		}
		if token != "--ro" && token != "--rw" {
			return aiLandlockCLI{}, fmt.Errorf("unknown argument %q", token)
		}
		if index >= len(arguments) || arguments[index] == "" {
			return aiLandlockCLI{}, fmt.Errorf("%s requires a path", token)
		}
		if token == "--ro" {
			parsed.readOnly = append(parsed.readOnly, arguments[index])
		} else {
			parsed.readWrite = append(parsed.readWrite, arguments[index])
		}
		index++
	}
	if len(parsed.command) == 0 || parsed.command[0] == "" {
		return aiLandlockCLI{}, errors.New("missing -- <argv> command")
	}
	return parsed, nil
}

func aiLandlockFSMask(abi uintptr) uint64 {
	mask := aiLandlockFSRefer - 1
	if abi >= 2 {
		mask |= aiLandlockFSRefer
	}
	if abi >= 3 {
		mask |= aiLandlockFSTruncate
	}
	if abi >= 5 {
		mask |= aiLandlockFSIoctlDev
	}
	return mask
}

func restrictAICommandLandlock(readOnly, readWrite []string) (bool, error) {
	abi, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, aiLandlockCreateRulesetVersion, 0, 0, 0)
	if errno != 0 {
		return false, fmt.Errorf("landlock is not enforced by this kernel: %w", errno)
	}
	if abi == 0 {
		return false, errors.New("landlock is not enforced by this kernel")
	}
	negotiated := abi
	if negotiated > aiLandlockMaximumABI {
		negotiated = aiLandlockMaximumABI
	}
	handled := aiLandlockFSMask(negotiated)
	attribute := aiLandlockRulesetAttr{HandledAccessFS: handled}
	ruleset, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attribute)), unsafe.Sizeof(attribute), 0, 0, 0, 0,
	)
	if errno != 0 {
		return false, errno
	}
	rulesetFD := int(ruleset)
	defer unix.Close(rulesetFD)
	readAccess := (aiLandlockFSExecute | aiLandlockFSReadFile | aiLandlockFSReadDir) & handled
	for _, path := range readOnly {
		if err := addAICommandLandlockRule(rulesetFD, path, readAccess); err != nil {
			return false, err
		}
	}
	for _, path := range readWrite {
		if err := addAICommandLandlockRule(rulesetFD, path, handled); err != nil {
			return false, err
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return false, err
	}
	_, _, errno = unix.Syscall6(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), 0, 0, 0, 0, 0)
	if errno != 0 {
		return false, errno
	}
	return abi < aiLandlockMaximumABI, nil
}

func addAICommandLandlockRule(rulesetFD int, path string, access uint64) error {
	pathFD, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("cannot open rule path %s: %w", path, err)
	}
	defer unix.Close(pathFD)
	var stat unix.Stat_t
	if err := unix.Fstat(pathFD, &stat); err == nil && stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= aiLandlockFSExecute | aiLandlockFSWriteFile | aiLandlockFSReadFile | aiLandlockFSTruncate | aiLandlockFSIoctlDev
	}
	attribute := aiLandlockPathBeneathAttr{AllowedAccess: access, ParentFD: int32(pathFD)}
	_, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFD), aiLandlockRulePathBeneath, uintptr(unsafe.Pointer(&attribute)), 0, 0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func failAICommandLandlock(prefix string, err error) int {
	if err == nil {
		fmt.Fprintf(os.Stderr, "landlock-run: %s\n", prefix)
	} else {
		fmt.Fprintf(os.Stderr, "landlock-run: %s: %v\n", prefix, err)
	}
	return aiLandlockRunnerFailureExit
}
