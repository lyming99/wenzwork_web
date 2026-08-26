//go:build !windows

package main

import "github.com/Kodecable/crosspty"

func startSupervisedPTY(config crosspty.CommandConfig) (processPTY, error) {
	pty, err := crosspty.Start(config)
	if err != nil {
		return nil, err
	}
	return pty, nil
}
