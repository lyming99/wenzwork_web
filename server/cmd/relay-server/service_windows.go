//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows/svc"
)

const (
	relayWindowsServiceName     = "WenzWorkRelay"
	relayWindowsServiceChildEnv = "WENZWORK_RELAY_SERVICE_CHILD"
)

// runAsWindowsServiceIfNeeded connects the process launched by the Service
// Control Manager to its dispatcher. A child process runs the regular Relay
// lifecycle so the cross-platform runtime and its signal handling stay in one
// place; the SCM parent owns service status and termination.
func runAsWindowsServiceIfNeeded() (bool, error) {
	if os.Getenv(relayWindowsServiceChildEnv) == "1" {
		return false, nil
	}
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, err
	}
	return true, svc.Run(relayWindowsServiceName, relayWindowsService{})
}

type relayWindowsService struct{}

func (relayWindowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending}
	executable, err := os.Executable()
	if err != nil {
		return true, 1
	}
	child := exec.Command(executable)
	child.Env = append(os.Environ(), relayWindowsServiceChildEnv+"=1")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return true, 1
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	accepted := svc.AcceptStop | svc.AcceptShutdown
	statuses <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- svc.Status{State: svc.Running, Accepts: accepted}
			case svc.Stop, svc.Shutdown:
				statuses <- svc.Status{State: svc.StopPending}
				if child.Process != nil {
					_ = child.Process.Kill()
				}
				<-done
				return false, 0
			}
		case err := <-done:
			if err == nil {
				return false, 0
			}
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				return true, uint32(exitError.ExitCode())
			}
			return true, 1
		}
	}
}
