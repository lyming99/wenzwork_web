package main

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeRawProcess deliberately has two physical pipes so tests cannot
// accidentally keep depending on a merged PTY-style stream.
type fakeRawProcess struct {
	stdoutReader *io.PipeReader
	stdoutWriter *io.PipeWriter
	stderrReader *io.PipeReader
	stderrWriter *io.PipeWriter
	exitReady    chan struct{}
	exitOnce     sync.Once
	mu           sync.Mutex
	exitCode     int
	pid          int
	closed       bool
}

func newFakeRawProcess(pid int) *fakeRawProcess {
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	return &fakeRawProcess{
		stdoutReader: stdoutReader, stdoutWriter: stdoutWriter,
		stderrReader: stderrReader, stderrWriter: stderrWriter,
		exitReady: make(chan struct{}), pid: pid,
	}
}

func (process *fakeRawProcess) Stdout() io.ReadCloser { return process.stdoutReader }
func (process *fakeRawProcess) Stderr() io.ReadCloser { return process.stderrReader }

func (process *fakeRawProcess) Wait() int {
	<-process.exitReady
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.exitCode
}

func (process *fakeRawProcess) Pid() int { return process.pid }

func (process *fakeRawProcess) Close() error {
	process.finish(-1)
	return nil
}

func (process *fakeRawProcess) emitStdout(contents []byte) error {
	_, err := process.stdoutWriter.Write(contents)
	return err
}

func (process *fakeRawProcess) emitStderr(contents []byte) error {
	_, err := process.stderrWriter.Write(contents)
	return err
}

func (process *fakeRawProcess) finish(exitCode int) {
	process.exitOnce.Do(func() {
		process.mu.Lock()
		process.exitCode = exitCode
		process.closed = exitCode < 0
		process.mu.Unlock()
		_ = process.stdoutWriter.Close()
		_ = process.stderrWriter.Close()
		close(process.exitReady)
	})
}

func (process *fakeRawProcess) isClosed() bool {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.closed
}

type fakeRawStarter struct {
	mu        sync.Mutex
	specs     []rawProcessLaunchSpec
	processes []*fakeRawProcess
	startErr  error
}

func (starter *fakeRawStarter) Start(spec rawProcessLaunchSpec) (rawProcessHandle, error) {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if starter.startErr != nil {
		return nil, starter.startErr
	}
	process := newFakeRawProcess(len(starter.processes) + 100)
	starter.specs = append(starter.specs, spec)
	starter.processes = append(starter.processes, process)
	return process, nil
}

func (starter *fakeRawStarter) latest() *fakeRawProcess {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	if len(starter.processes) == 0 {
		return nil
	}
	return starter.processes[len(starter.processes)-1]
}

func fakeRawStarterSnapshot(starter *fakeRawStarter) ([]rawProcessLaunchSpec, []*fakeRawProcess) {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	return append([]rawProcessLaunchSpec(nil), starter.specs...), append([]*fakeRawProcess(nil), starter.processes...)
}

func TestRawProcessSupervisorSeparatesStreamsAndReleasesCapacity(t *testing.T) {
	starter := new(fakeRawStarter)
	supervisor := newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	supervisor.memoryPollInterval = time.Hour
	root := t.TempDir()
	process, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root, Argv: []string{"runner"},
		Limits: processResourceLimits{MaximumLifetime: time.Minute, MaximumMemoryBytes: 1, MaximumOutputBytes: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root, Argv: []string{"runner"},
		Limits: processResourceLimits{MaximumLifetime: time.Minute, MaximumMemoryBytes: 1, MaximumOutputBytes: 1024},
	}); err != errRPCBusy {
		t.Fatalf("capacity error = %v", err)
	}
	fake := starter.latest()
	stdoutDone, stderrDone := make(chan []byte, 1), make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(process.Stdout()); stdoutDone <- data }()
	go func() { data, _ := io.ReadAll(process.Stderr()); stderrDone <- data }()
	if err := fake.emitStdout([]byte("out")); err != nil {
		t.Fatal(err)
	}
	if err := fake.emitStderr([]byte("err")); err != nil {
		t.Fatal(err)
	}
	fake.finish(0)
	if got := process.Wait(); got != 0 {
		t.Fatalf("exit = %d", got)
	}
	if got := string(<-stdoutDone); got != "out" {
		t.Fatalf("stdout = %q", got)
	}
	if got := string(<-stderrDone); got != "err" {
		t.Fatalf("stderr = %q", got)
	}
	process.release()
}

func TestRawProcessSupervisorAllowsTaskSchedulerToIgnoreGenericCapacity(t *testing.T) {
	starter := new(fakeRawStarter)
	supervisor := newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	supervisor.memoryPollInterval = time.Hour
	t.Cleanup(func() { _ = supervisor.Close() })
	root := t.TempDir()
	spec := rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root, Argv: []string{"runner"},
		IgnoreConcurrencyLimit: true,
		Limits:                 processResourceLimits{MaximumLifetime: time.Minute, MaximumMemoryBytes: 1, MaximumOutputBytes: 1024},
	}
	first, err := supervisor.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := supervisor.Start(spec)
	if err != nil {
		t.Fatalf("task scheduler was limited by generic process capacity: %v", err)
	}
	first.release()
	second.release()
}

func TestRawProcessSupervisorStopsProcessWhenCombinedRawOutputExceedsLimit(t *testing.T) {
	starter := new(fakeRawStarter)
	supervisor := newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	supervisor.memoryPollInterval = time.Hour
	root := t.TempDir()
	process, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root, Argv: []string{"runner"},
		Limits: processResourceLimits{MaximumLifetime: time.Minute, MaximumMemoryBytes: 1, MaximumOutputBytes: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer process.release()
	fake := starter.latest()
	stdoutDone := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(process.Stdout()); stdoutDone <- data }()
	if err := fake.emitStdout([]byte("four")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !fake.isClosed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !fake.isClosed() || process.reason() != "output_limit" {
		t.Fatalf("output-limited process closed=%v reason=%q", fake.isClosed(), process.reason())
	}
	if exitCode := process.Wait(); exitCode != -1 {
		t.Fatalf("output-limited exit code = %d", exitCode)
	}
	if output := string(<-stdoutDone); output != "four" {
		t.Fatalf("stdout was not preserved before cancellation: %q", output)
	}
}

func TestMergeInteractiveCommandEnvironmentKeepsUserCLIPathsAndDropsSecrets(t *testing.T) {
	merged := mergeInteractiveCommandEnvironment(
		[]string{
			"Path=C:\\Users\\alice\\AppData\\Roaming\\npm;C:\\Program Files\\nodejs",
			"USERPROFILE=C:\\Users\\alice",
			"APPDATA=C:\\Users\\alice\\AppData\\Roaming",
			"XDG_CONFIG_HOME=/home/alice/.config",
			"LANG=zh_CN.UTF-8",
			"API_TOKEN=must-not-leak",
		},
		[]string{
			"PATH=C:\\Windows\\System32",
			"USERPROFILE=C:\\Windows\\System32\\config\\systemprofile",
			"XDG_CONFIG_HOME=/untrusted/config",
			"TASK_LABEL=reviewed",
			"WENZWORK_TASK_EXEC_ARG_000=private-argv",
		},
	)
	values := make(map[string]string, len(merged))
	for _, value := range merged {
		name, contents, found := strings.Cut(value, "=")
		if !found {
			t.Fatalf("invalid merged environment entry %q", value)
		}
		values[strings.ToUpper(name)] = contents
	}
	if got := values["PATH"]; got != "C:\\Users\\alice\\AppData\\Roaming\\npm;C:\\Program Files\\nodejs" {
		t.Fatalf("PATH = %q", got)
	}
	if got := values["USERPROFILE"]; got != "C:\\Users\\alice" {
		t.Fatalf("USERPROFILE = %q", got)
	}
	if values["XDG_CONFIG_HOME"] != "/home/alice/.config" || values["LANG"] != "zh_CN.UTF-8" {
		t.Fatalf("portable runtime environment was lost: %#v", values)
	}
	if values["TASK_LABEL"] != "reviewed" || values["WENZWORK_TASK_EXEC_ARG_000"] != "private-argv" {
		t.Fatalf("reviewed task environment was lost: %#v", values)
	}
	if _, found := values["API_TOKEN"]; found {
		t.Fatalf("unreviewed interactive secret leaked: %#v", values)
	}
}

func TestFilterCommandRuntimeEnvironmentUsesClosedAllowlist(t *testing.T) {
	filtered := filterCommandRuntimeEnvironment([]string{
		"PATH=/usr/local/bin:/usr/bin",
		"HOME=/home/alice",
		"SSH_AUTH_SOCK=/run/user/1000/ssh-agent.sock",
		"OPENAI_API_KEY=must-not-leak",
		"MALFORMED",
	})
	joined := strings.Join(filtered, "\n")
	for _, expected := range []string{"PATH=", "HOME=", "SSH_AUTH_SOCK="} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("filtered runtime environment %q does not contain %q", joined, expected)
		}
	}
	if strings.Contains(joined, "OPENAI_API_KEY") || strings.Contains(joined, "MALFORMED") {
		t.Fatalf("unreviewed runtime environment leaked: %q", joined)
	}
}
