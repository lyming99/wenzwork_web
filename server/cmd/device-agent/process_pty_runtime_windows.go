//go:build windows

package main

import (
	"fmt"
	"syscall"

	"github.com/Kodecable/crosspty"
)

// startSupervisedPTY creates all Agent-owned interactive terminal processes.
// ConPTY routes I/O through its pseudo console, so no real console window is
// needed.  Without HideWindow, Windows can briefly create a visible black
// shell window while launching cmd.exe or PowerShell on the target device.
func startSupervisedPTY(config crosspty.CommandConfig) (processPTY, error) {
	desktop, err := windowsSupervisedPTYDesktopName()
	if err != nil {
		return nil, fmt.Errorf("prepare hidden terminal desktop: %w", err)
	}
	config.WindowsDesktop = desktop
	pty, err := crosspty.StartWithSysProcAttr(config, windowsSupervisedPTYAttributes())
	if err != nil {
		return nil, err
	}
	return pty, nil
}

func windowsSupervisedPTYAttributes() *syscall.SysProcAttr {
	// CREATE_NO_WINDOW is intentionally not used for ConPTY: it prevents the
	// pseudo-console child from producing output on supported Windows builds.
	// ConPTY itself is the no-window terminal implementation; HideWindow also
	// suppresses any shell startup UI before it attaches to that pseudoconsole.
	return &syscall.SysProcAttr{HideWindow: true}
}
