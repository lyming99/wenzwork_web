//go:build windows

package crosspty

import (
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// sys must not be nil.
func (p *ptyWin) createProcess(cc CommandConfig, sys *syscall.SysProcAttr) error {
	pi := new(windows.ProcessInformation)

	// Need EXTENDED_STARTUPINFO_PRESENT as we're making use of the attribute list field.
	flags := sys.CreationFlags | uint32(windows.CREATE_UNICODE_ENVIRONMENT) | windows.EXTENDED_STARTUPINFO_PRESENT
	paused := false
	if p.closeCfg.KillMode == KillModeKillGroupOnClose || p.closeCfg.KillMode == KillModeKillGroupOnSubProcessExit {
		flags = flags | windows.CREATE_SUSPENDED
		paused = true
	}

	siEx, err := p.createStartupInfoEx(sys, cc.WindowsDesktop)
	if err != nil {
		return err
	}

	envBlock := createEnvBlock(dedupEnvCase(true, cc.Env))

	cmdline := sys.CmdLine
	if cmdline == "" {
		cmdline = makeCmdLine(cc.Argv)
	}

	cmdlineU16, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return err
	}

	dirU16, err := windows.UTF16PtrFromString(cc.Dir)
	if err != nil {
		return err
	}

	argv0U16, err := windows.UTF16PtrFromString(cc.Argv[0])
	if err != nil {
		return err
	}

	if sys.Token != 0 {
		err = windows.CreateProcessAsUser(
			windows.Token(sys.Token),
			argv0U16,
			cmdlineU16,
			nil,
			nil,
			false,
			flags,
			envBlock,
			dirU16,
			&siEx.StartupInfo,
			pi,
		)
	} else {
		err = windows.CreateProcess(
			argv0U16,
			cmdlineU16,
			nil,
			nil,
			false,
			flags,
			envBlock,
			dirU16,
			&siEx.StartupInfo,
			pi,
		)
	}
	if err != nil {
		return err
	}
	defer windows.CloseHandle(pi.Thread)

	p.processId = pi.ProcessId
	p.processHandle = pi.Process

	err = makeConPTYAutoCloseOutputPipe(p.conPty)
	if err != nil {
		windows.TerminateProcess(p.processHandle, 0)
		return err
	}

	if p.closeCfg.KillMode != KillModeKillSubProcess {
		err = windows.AssignProcessToJobObject(p.jobHandle, p.processHandle)
		if err != nil {
			windows.TerminateProcess(p.processHandle, 0)
			return err
		}
	}

	if paused {
		_, err = windows.ResumeThread(pi.Thread)
		if err != nil {
			windows.TerminateProcess(p.processHandle, 0)
			return err
		}
	}

	go p.processWaiter()
	return nil
}

func (p *ptyWin) processWaiter() {
	defer close(p.exitch)

	event, err := windows.WaitForSingleObject(windows.Handle(p.processHandle), windows.INFINITE)
	if err != nil || event != windows.WAIT_OBJECT_0 {
		p.exitCode = -1
		return
	}

	var exitCode uint32
	err = windows.GetExitCodeProcess(windows.Handle(p.processHandle), &exitCode)
	if err != nil {
		p.exitCode = -1
	} else {
		p.exitCode = int(exitCode)
		if p.closeCfg.KillMode == KillModeKillGroupOnSubProcessExit {
			windows.TerminateJobObject(p.jobHandle, p.closeCfg.KillExitCode)
		}
	}
}

func (p *ptyWin) createStartupInfoEx(sys *syscall.SysProcAttr, desktop string) (*windows.StartupInfoEx, error) {
	siEx := new(windows.StartupInfoEx)
	siEx.Flags = windows.STARTF_USESTDHANDLES
	if desktop != "" {
		var err error
		siEx.Desktop, err = windows.UTF16PtrFromString(desktop)
		if err != nil {
			return nil, err
		}
	}

	if sys.HideWindow {
		siEx.Flags |= syscall.STARTF_USESHOWWINDOW
		siEx.ShowWindow = syscall.SW_HIDE
	}

	var err error
	p.attrList, err = p.createProcThreadAttList()
	if err != nil {
		return nil, err
	}
	siEx.ProcThreadAttributeList = p.attrList.List()

	siEx.Cb = uint32(unsafe.Sizeof(*siEx))

	return siEx, nil
}

// Copied from photostorm/pty (originally from the Go standard library).
func makeCmdLine(args []string) string {
	var s string
	for _, v := range args {
		if s != "" {
			s += " "
		}
		s += windows.EscapeArg(v)
	}
	return s
}

// Adapted from photostorm/pty.
func defaultEnvByToken(token syscall.Token) (env []string, err error) {
	var block *uint16
	err = windows.CreateEnvironmentBlock(&block, windows.Token(token), false)
	if err != nil {
		return nil, err
	}

	defer windows.DestroyEnvironmentBlock(block)
	blockp := unsafe.Pointer(block)

	for {

		// find NUL terminator
		end := blockp
		for *(*uint16)(end) != 0 {
			end = unsafe.Pointer(uintptr(end) + 2)
		}

		n := (uintptr(end) - uintptr(blockp)) / 2
		if n == 0 {
			// environment block ends with empty string
			break
		}

		entry := (*[(1 << 30) - 1]uint16)(blockp)[:n:n]
		env = append(env, string(utf16.Decode(entry)))
		blockp = unsafe.Pointer(uintptr(blockp) + (2 * (uintptr(len(entry)) + 1)))
	}
	return
}

// Copied from photostorm/pty (originally from the Go standard library).
func dedupEnvCase(caseInsensitive bool, env []string) []string {
	// Construct the output in reverse order, to preserve the
	// last occurrence of each key.
	out := make([]string, 0, len(env))
	saw := make(map[string]bool, len(env))
	for n := len(env); n > 0; n-- {
		kv := env[n-1]

		i := strings.Index(kv, "=")
		if i == 0 {
			// We observe in practice keys with a single leading "=" on Windows.
			// TODO(#49886): Should we consume only the first leading "=" as part
			// of the key, or parse through arbitrarily many of them until a non-"="?
			i = strings.Index(kv[1:], "=") + 1
		}
		if i < 0 {
			if kv != "" {
				// The entry is not of the form "key=value" (as it is required to be).
				// Leave it as-is for now.
				// TODO(#52436): should we strip or reject these bogus entries?
				out = append(out, kv)
			}
			continue
		}
		k := kv[:i]
		if caseInsensitive {
			k = strings.ToLower(k)
		}
		if saw[k] {
			continue
		}

		saw[k] = true
		out = append(out, kv)
	}

	// Now reverse the slice to restore the original order.
	for i := 0; i < len(out)/2; i++ {
		j := len(out) - i - 1
		out[i], out[j] = out[j], out[i]
	}

	return out
}

// Copied from photostorm/pty (originally from the Go standard library).
func createEnvBlock(envv []string) *uint16 {
	if len(envv) == 0 {
		return &utf16.Encode([]rune("\x00\x00"))[0]
	}
	length := 0
	for _, s := range envv {
		length += len(s) + 1
	}
	length += 1

	b := make([]byte, length)
	i := 0
	for _, s := range envv {
		l := len(s)
		copy(b[i:i+l], []byte(s))
		copy(b[i+l:i+l+1], []byte{0})
		i = i + l + 1
	}
	copy(b[i:i+1], []byte{0})

	return &utf16.Encode([]rune(string(b)))[0]
}
