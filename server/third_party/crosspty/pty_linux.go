//go:build linux

package crosspty

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

type PtyLinux interface {
	Pty

	// PidFD returns the Linux pidfd tracked by this package.
	// The returned fd is owned by the Pty instance and remains valid until Close().
	// It returns -1 when pidfd is not available.
	PidFD() int
}

func (p *ptyUnix) PidFD() int {
	return p.pidFD
}

func (p *ptyUnix) setSysProcAttr(cmd *exec.Cmd) {
	p.pidFD = -1
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.PidFD = &p.pidFD
}

func (p *ptyUnix) signal(group bool, signal syscall.Signal) (err error) {
	const PIDFD_SIGNAL_PROCESS_GROUP = 4 // (since linux 6.9)

	if p.pidFD == -1 {
		return p.signalUnix(group, signal)
	} else {
		if group {
			err = unix.PidfdSendSignal(p.pidFD, signal, nil, PIDFD_SIGNAL_PROCESS_GROUP)
			if errors.Is(err, syscall.EINVAL) {
				return p.signalUnix(group, signal)
			}
			return err
		} else {
			return unix.PidfdSendSignal(p.pidFD, signal, nil, 0)
		}
	}
}

func closePidFD(pidFd int) {
	if pidFd != -1 {
		syscall.Close(pidFd)
	}
}
