//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestRunPlatformServiceDispatchesOnlyTheNativeServiceCommand(t *testing.T) {
	handled, err := runPlatformService([]string{"serve"}, io.Discard)
	if handled || err != nil {
		t.Fatalf("serve dispatch handled=%v error=%v", handled, err)
	}
	handled, err = runPlatformService([]string{"service"}, nil)
	if !handled || err == nil {
		t.Fatalf("nil service stderr handled=%v error=%v", handled, err)
	}
}

func TestWindowsServiceReportsLifecycleAndCancelsOnStop(t *testing.T) {
	started := make(chan struct{})
	service := &deviceAgentWindowsService{
		arguments: []string{"--env-file", "managed.env"},
		stderr:    io.Discard,
		run: func(ctx context.Context, arguments []string, stderr io.Writer) error {
			if len(arguments) != 2 || arguments[0] != "--env-file" || arguments[1] != "managed.env" || stderr != io.Discard {
				return errors.New("service arguments were not preserved")
			}
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	requests := make(chan svc.ChangeRequest, 2)
	statuses := make(chan svc.Status, 4)
	result := make(chan struct {
		specific bool
		code     uint32
	}, 1)
	go func() {
		specific, code := service.Execute(nil, requests, statuses)
		result <- struct {
			specific bool
			code     uint32
		}{specific: specific, code: code}
	}()

	assertServiceStatus(t, statuses, svc.StartPending, 0)
	assertServiceStatus(t, statuses, svc.Running, svc.AcceptStop|svc.AcceptShutdown)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("service runner did not start")
	}
	requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
	assertServiceStatus(t, statuses, svc.Running, svc.AcceptStop|svc.AcceptShutdown)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	assertServiceStatus(t, statuses, svc.StopPending, 0)

	select {
	case actual := <-result:
		if actual.specific || actual.code != 0 {
			t.Fatalf("clean service stop specific=%v code=%d", actual.specific, actual.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not stop after cancellation")
	}
}

func TestWindowsServiceReturnsFailureForServeError(t *testing.T) {
	service := &deviceAgentWindowsService{
		stderr: io.Discard,
		run: func(context.Context, []string, io.Writer) error {
			return errors.New("startup failed")
		},
	}
	statuses := make(chan svc.Status, 2)
	specific, code := service.Execute(nil, make(chan svc.ChangeRequest), statuses)
	if specific || code != 1 {
		t.Fatalf("failed service specific=%v code=%d", specific, code)
	}
	assertServiceStatus(t, statuses, svc.StartPending, 0)
	assertServiceStatus(t, statuses, svc.Running, svc.AcceptStop|svc.AcceptShutdown)
}

func assertServiceStatus(t *testing.T, statuses <-chan svc.Status, state svc.State, accepts svc.Accepted) {
	t.Helper()
	select {
	case status := <-statuses:
		if status.State != state || status.Accepts != accepts {
			t.Fatalf("service status state=%v accepts=%v, want state=%v accepts=%v", status.State, status.Accepts, state, accepts)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("service status %v was not reported", state)
	}
}
