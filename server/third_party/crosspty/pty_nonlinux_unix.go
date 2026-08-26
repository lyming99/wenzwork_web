//go:build unix && !linux

package crosspty

import (
	"os/exec"
	"syscall"
)

func (p *ptyUnix) setSysProcAttr(_ *exec.Cmd) {
	p.pidFD = -1
}

func (p *ptyUnix) signal(group bool, signal syscall.Signal) error {
	return p.signalUnix(group, signal)
}

func closePidFD(pidFd int) {
}
