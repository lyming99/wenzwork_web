package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kodecable/crosspty"
	"github.com/google/uuid"
)

type fakeProcessPTY struct {
	reader *io.PipeReader
	writer *io.PipeWriter
	pid    int

	mu        sync.Mutex
	writes    [][]byte
	resizes   []crosspty.TermSize
	closed    bool
	exitCode  int
	exitReady chan struct{}
	exitOnce  sync.Once
}

func newFakeProcessPTY(pid int) *fakeProcessPTY {
	reader, writer := io.Pipe()
	return &fakeProcessPTY{reader: reader, writer: writer, pid: pid, exitCode: -1, exitReady: make(chan struct{})}
}

func (process *fakeProcessPTY) Read(buffer []byte) (int, error) { return process.reader.Read(buffer) }

func (process *fakeProcessPTY) Write(buffer []byte) (int, error) {
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.closed {
		return 0, io.ErrClosedPipe
	}
	process.writes = append(process.writes, append([]byte(nil), buffer...))
	return len(buffer), nil
}

func (process *fakeProcessPTY) Resize(size crosspty.TermSize) error {
	process.mu.Lock()
	defer process.mu.Unlock()
	if process.closed {
		return io.ErrClosedPipe
	}
	process.resizes = append(process.resizes, size)
	return nil
}

func (process *fakeProcessPTY) Close() error {
	process.finish(-1)
	return nil
}

func (process *fakeProcessPTY) Wait() int {
	<-process.exitReady
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.exitCode
}

func (process *fakeProcessPTY) Pid() int { return process.pid }

func (process *fakeProcessPTY) emit(contents []byte) error {
	_, err := process.writer.Write(contents)
	return err
}

func (process *fakeProcessPTY) finish(exitCode int) {
	process.exitOnce.Do(func() {
		process.mu.Lock()
		process.closed = true
		process.exitCode = exitCode
		process.mu.Unlock()
		_ = process.writer.Close()
		close(process.exitReady)
	})
}

func (process *fakeProcessPTY) snapshotWrites() [][]byte {
	process.mu.Lock()
	defer process.mu.Unlock()
	result := make([][]byte, len(process.writes))
	for index := range process.writes {
		result[index] = append([]byte(nil), process.writes[index]...)
	}
	return result
}

func (process *fakeProcessPTY) isClosed() bool {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.closed
}

type fakePTYStarter struct {
	mu        sync.Mutex
	specs     []processLaunchSpec
	processes []*fakeProcessPTY
}

func (starter *fakePTYStarter) Start(spec processLaunchSpec) (processPTY, error) {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	process := newFakeProcessPTY(1000 + len(starter.processes))
	starter.specs = append(starter.specs, spec)
	starter.processes = append(starter.processes, process)
	return process, nil
}

func (starter *fakePTYStarter) latest() *fakeProcessPTY {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if len(starter.processes) == 0 {
		return nil
	}
	return starter.processes[len(starter.processes)-1]
}

func TestProcessSupervisorEnforcesRootConcurrencyOutputAndLifetime(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	starter := new(fakePTYStarter)
	supervisor := newProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	base := processLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: child,
		Argv: []string{filepath.Join(root, "fake-shell")}, Environment: []string{"TASK_LABEL=reviewed"}, Rows: 24, Columns: 80,
		Limits: processResourceLimits{MaximumLifetime: time.Minute, MaximumMemoryBytes: 1024, MaximumOutputBytes: 4},
	}
	process, err := supervisor.Start(base)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(starter.specs[0].Environment, "TASK_LABEL=reviewed") {
		t.Fatalf("reviewed task environment was not preserved: %#v", starter.specs[0].Environment)
	}
	forbidden := base
	forbidden.Environment = []string{"PATH=untrusted"}
	if _, err := supervisor.Start(forbidden); err != errRPCInvalid {
		t.Fatalf("protected task environment Start() error = %v", err)
	}
	if _, err := supervisor.Start(base); err != errRPCBusy {
		t.Fatalf("concurrent Start() error = %v", err)
	}
	outside := base
	outside.WorkingDirectory = t.TempDir()
	if _, err := newProcessSupervisorWithDependencies(starter, nil, 1).Start(outside); err != errRPCInvalid {
		t.Fatalf("outside-root Start() error = %v", err)
	}
	fake := starter.latest()
	go func() { _ = fake.emit([]byte("12345")) }()
	buffer := make([]byte, 8)
	if n, err := process.Read(buffer); err != nil || n != 5 {
		t.Fatalf("Read() = %d, %v", n, err)
	}
	eventually(t, time.Second, fake.isClosed)
	process.release()

	unlimited := base
	unlimited.Limits.MaximumLifetime = 0
	unlimited.Limits.MaximumOutputBytes = 1024
	longRunning, err := supervisor.Start(unlimited)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if starter.latest().isClosed() {
		t.Fatal("zero maximum lifetime closed an unlimited process")
	}
	if err := longRunning.Close("client_close"); err != nil {
		t.Fatal(err)
	}
	longRunning.release()

	lifetime := base
	lifetime.Limits.MaximumLifetime = 20 * time.Millisecond
	lifetime.Limits.MaximumOutputBytes = 1024
	short, err := supervisor.Start(lifetime)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, starter.latest().isClosed)
	short.release()
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProcessSupervisorEnforcesProcessTreeMemoryAndAgentExitCleanup(t *testing.T) {
	root := t.TempDir()
	starter := new(fakePTYStarter)
	var memoryBytes atomic.Uint64
	supervisor := newProcessSupervisorWithDependencies(starter, func(int) (uint64, error) {
		return memoryBytes.Load(), nil
	}, 2)
	supervisor.memoryPollInterval = time.Millisecond
	spec := processLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root,
		Argv: []string{filepath.Join(root, "fake-shell")}, Rows: 24, Columns: 80,
		Limits: processResourceLimits{
			MaximumLifetime: time.Minute, MaximumMemoryBytes: 1024, MaximumOutputBytes: 1024,
		},
	}
	memoryLimited, err := supervisor.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	memoryBytes.Store(1025)
	eventually(t, time.Second, starter.latest().isClosed)
	if memoryLimited.reason() != "memory_limit" {
		t.Fatalf("memory-limited close reason = %q", memoryLimited.reason())
	}
	memoryLimited.release()

	memoryBytes.Store(0)
	agentOwned, err := supervisor.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
	if !starter.latest().isClosed() || agentOwned.reason() != "agent_exit" {
		t.Fatalf("Agent exit cleanup closed=%v reason=%q", starter.latest().isClosed(), agentOwned.reason())
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true before timeout")
		}
		time.Sleep(time.Millisecond)
	}
}
