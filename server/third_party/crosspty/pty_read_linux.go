//go:build linux

package crosspty

import (
	"errors"
	"io"
	"syscall"
)

func (p *ptyUnix) Read(d []byte) (n int, err error) {
	n, err = p.file.Read(d)

	// Linux kernel is returning EIO when reading a dead pty slave
	// https://github.com/creack/pty/issues/21#issuecomment-129381749
	if errors.Is(err, syscall.EIO) {
		err = io.EOF
	}

	return n, err
}
