//go:build unix && !linux

package crosspty

func (p *ptyUnix) Read(d []byte) (n int, err error) {
	n, err = p.file.Read(d)
	return n, err
}
