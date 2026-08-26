//go:build windows

package crosspty

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

type ptyWin struct {
	conPty    windows.Handle
	readPipe  *os.File
	writePipe *os.File
	attrList  *windows.ProcThreadAttributeListContainer

	closeCfg CloseConfig

	exitCode int
	exitch   chan any
	closer   sync.Once

	processId     uint32
	processHandle windows.Handle

	jobHandle windows.Handle
}

func start(cc CommandConfig) (Pty, error) {
	return StartWithSysProcAttr(cc, &syscall.SysProcAttr{})
}

// Windows only.
// The following SysProcAttr fields are handled; others are ignored.
//   - HideWindow
//   - CmdLine
//   - CreationFlags
//   - Token
//
// We behavior may differ from Go's std lib.
// If Token != 0, the default value of CommandConfig.Env is obtained from
// CreateEnvironmentBlock using the given Token with bInherit = false.
func StartWithSysProcAttr(cc CommandConfig, sys *syscall.SysProcAttr) (Pty, error) {
	if sys == nil {
		sys = &syscall.SysProcAttr{}
	}

	if sys.Token != 0 && cc.Env == nil {
		var err error
		cc.Env, err = defaultEnvByToken(sys.Token)
		if err != nil {
			return nil, err
		}
	}

	cc, err := NormalizeCommandConfig(cc)
	if err != nil {
		return nil, err
	}

	p := &ptyWin{
		exitch:   make(chan any),
		closeCfg: cc.CloseConfig,
	}

	if p.closeCfg.KillMode != KillModeKillSubProcess {
		p.jobHandle, err = windows.CreateJobObject(nil, nil)
		if err != nil {
			return nil, err
		}
	}

	err = p.openConPTY(cc.Size)
	if err != nil {
		windows.CloseHandle(p.jobHandle)
		return nil, err
	}

	err = p.createProcess(cc, sys)
	if err != nil {
		windows.ClosePseudoConsole(p.conPty)
		p.readPipe.Close()
		p.writePipe.Close()
		p.attrList.Delete()
		windows.CloseHandle(p.jobHandle)
		return nil, err
	}

	return p, err
}

func (p *ptyWin) killProcess() error {
	p.writePipe.Close() // trigger CTRL_CLOSE_EVENT

	childExited := false
	select {
	case <-time.After(p.closeCfg.KillDelay):
		break
	case <-p.exitch:
		childExited = true
	}

	if p.closeCfg.KillMode == KillModeKillSubProcess {
		if childExited {
			return nil
		}
		// doc: https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-terminateprocess
		err := windows.TerminateProcess(p.processHandle, p.closeCfg.KillExitCode)
		if err != nil {
			// > After a process has terminated, call to TerminateProcess with
			// > open handles to the process fails with ERROR_ACCESS_DENIED (5)
			// > error code.
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return err
			}
		}
	} else {
		err := windows.TerminateJobObject(p.jobHandle, p.closeCfg.KillExitCode)
		if err != nil {
			return err
		}
	}

	if childExited {
		return nil
	}

	select {
	case <-time.After(p.closeCfg.CloseTimeout - p.closeCfg.KillDelay):
		return ErrKillTimeout
	case <-p.exitch:
		return nil
	}
}

func (p *ptyWin) Close() (err error) {
	p.closer.Do(func() {
		err = p.killProcess()
		p.attrList.Delete()
		windows.CloseHandle(p.processHandle)
		windows.ClosePseudoConsole(p.conPty)
		p.readPipe.Close()
		windows.CloseHandle(p.jobHandle)
	})
	return
}

func (p *ptyWin) Wait() int {
	<-p.exitch
	return p.exitCode
}

func (p *ptyWin) Read(d []byte) (n int, err error) {
	n, err = p.readPipe.Read(d)
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) {
		err = io.EOF
	}
	return
}

func (p *ptyWin) Write(d []byte) (n int, err error) {
	var n32 uint32
	err = windows.WriteFile(windows.Handle(p.writePipe.Fd()), d, &n32, nil)
	return int(n32), err
}

func (p *ptyWin) Pid() int {
	return int(p.processId)
}

func (p *ptyWin) Resize(sz TermSize) error {
	return windows.ResizePseudoConsole(p.conPty, windowsCoord(sz))
}
