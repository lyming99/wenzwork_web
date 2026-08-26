//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"time"

	"golang.org/x/sys/windows/svc"
)

const windowsServiceName = "WenzWorkDeviceAgent"

type deviceAgentWindowsService struct {
	arguments []string
	stderr    io.Writer
	run       func(context.Context, []string, io.Writer) error
}

func runPlatformService(arguments []string, stderr io.Writer) (bool, error) {
	if len(arguments) == 0 || arguments[0] != "service" {
		return false, nil
	}
	if stderr == nil {
		return true, errors.New("service error output is required")
	}
	return true, svc.Run(windowsServiceName, &deviceAgentWindowsService{
		arguments: append([]string(nil), arguments[1:]...),
		stderr:    stderr,
	})
}

func (service *deviceAgentWindowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status := svc.Status{State: svc.StartPending}
	statuses <- status

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	runner := service.run
	if runner == nil {
		runner = runServeContext
	}
	go func() {
		completed <- runner(ctx, service.arguments, service.stderr)
	}()

	status = svc.Status{State: svc.Running, Accepts: accepted}
	statuses <- status
	for {
		select {
		case err := <-completed:
			if err != nil && !errors.Is(err, context.Canceled) {
				return false, 1
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- status
			case svc.Stop, svc.Shutdown:
				status = svc.Status{State: svc.StopPending}
				statuses <- status
				cancel()
				select {
				case err := <-completed:
					if err != nil && !errors.Is(err, context.Canceled) {
						return false, 1
					}
					return false, 0
				case <-time.After(40 * time.Second):
					return false, 1
				}
			}
		}
	}
}
