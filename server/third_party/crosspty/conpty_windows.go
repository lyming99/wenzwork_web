//go:build windows

package crosspty

import (
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole  = kernel32.NewProc("CreatePseudoConsole")
	procReleasePseudoConsole = kernel32.NewProc("ReleasePseudoConsole")
)

func IsConPTYSupported() bool {
	if procCreatePseudoConsole.Find() != nil {
		return false
	}
	return true
}

func windowsCoord(sz TermSize) windows.Coord {
	return windows.Coord{X: int16(sz.Cols), Y: int16(sz.Rows)}
}

func (p *ptyWin) openConPTY(sz TermSize) error {
	if !IsConPTYSupported() {
		return ErrConPTYNotSupported
	}

	var err error
	var subprocessReadPipe *os.File
	var subprocessWritePipe *os.File

	p.readPipe, subprocessWritePipe, err = os.Pipe()
	if err != nil {
		return err
	}
	subprocessReadPipe, p.writePipe, err = os.Pipe()
	if err != nil {
		p.readPipe.Close()
		subprocessWritePipe.Close()
		return err
	}

	err = windows.CreatePseudoConsole(
		windowsCoord(sz),
		windows.Handle(subprocessReadPipe.Fd()),
		windows.Handle(subprocessWritePipe.Fd()),
		0, &p.conPty)
	if err != nil {
		p.readPipe.Close()
		p.writePipe.Close()
		subprocessWritePipe.Close()
		subprocessReadPipe.Close()
		return err
	}

	// ConPTY duplicates these handles internally and will release them when appropriate.
	// We can safely close our copies here without worrying about their lifetime.
	subprocessWritePipe.Close()
	subprocessReadPipe.Close()

	return nil
}

func (p *ptyWin) createProcThreadAttList() (attrList *windows.ProcThreadAttributeListContainer, err error) {
	attrList, err = windows.NewProcThreadAttributeList(1)
	if err != nil {
		return
	}

	// (*(*unsafe.Pointer)(unsafe.Pointer(&p.conPty))) -> (unsafe.Pointer(uintptr(conpty)))
	err = attrList.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, (*(*unsafe.Pointer)(unsafe.Pointer(&p.conPty))), uintptr(unsafe.Sizeof(p.conPty)))
	return
}

// see https://github.com/microsoft/terminal/blob/c4fbb58f69b0a5cc86c245505311d0a0b3cc1399/src/winconpty/winconpty.h#L11
type oldWindowsConPTYLayout struct {
	hSignal        windows.Handle
	hPtyReference  windows.Handle
	hConPtyProcess windows.Handle
}

// see https://learn.microsoft.com/en-us/windows/console/releasepseudoconsole
// > The HPCON handle owned by your application keeps the pseudoconsole session alive indefinitely by default.
// This causes the read pipe to block indefinitely after the subprocess exits,
// which differs from the Unix PTY behavior.
// > After calling this function, the pseudoconsole will automatically exit once all clients have disconnected.
// > All you need to do now is to read from or write to your output and input pipe handles until they return a failure.
func makeConPTYAutoCloseOutputPipe(conpty windows.Handle) error {
	if err := procReleasePseudoConsole.Find(); err == nil {
		// Only Windows 11 24H2 (build 26100) / Windows Server 2025 (build 26100) or later.
		// 6 years after ConPTY released. well, good job.

		// > The call is not expected to fail unless the hPC argument is invalid
		r0, _, _ := procReleasePseudoConsole.Call(uintptr(conpty))
		if r0 != 0 { // HRESULT
			return syscall.Errno(r0)
		}
		return nil
	}

	// Hacking to the gate!
	// see https://github.com/microsoft/terminal/discussions/19112#discussioncomment-13713590
	// > For older versions of Windows, I recommend copying our winconpty code
	// > and using it directly on the HPCON handle. The HPCON handle may
	// > change in future versions of Windows, but it's very safe to assume
	// > that old versions of Windows will not change it anymore.
	// see https://github.com/microsoft/terminal/blob/c4fbb58f69b0a5cc86c245505311d0a0b3cc1399/src/winconpty/winconpty.cpp#L532-L559

	if conpty == 0 || conpty == windows.InvalidHandle {
		return errors.New("invalid ConPTY handle")
	}

	// (*(*unsafe.Pointer)(unsafe.Pointer(&p.conPty))) -> (unsafe.Pointer(uintptr(conpty)))
	// "Less is exponentially more." So we do not have single-line suppression at 2026. Cool.
	pRealPty := (*oldWindowsConPTYLayout)(*(*unsafe.Pointer)(unsafe.Pointer(&conpty)))

	if pRealPty.hPtyReference != 0 && pRealPty.hPtyReference != windows.InvalidHandle {
		windows.CloseHandle(pRealPty.hPtyReference)
		pRealPty.hPtyReference = 0
	}

	return nil
}
