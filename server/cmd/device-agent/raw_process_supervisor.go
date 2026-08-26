package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Raw processes are intentionally separate from processSupervisor.  The
// latter owns a PTY/ConPTY and must only be used for interactive terminals.
// Background tasks, runner probes, and AI workspace commands use this type so
// stdout and stderr remain independent byte streams.
const defaultMaximumRawProcesses = defaultMaximumSupervisedProcesses

var errTaskExecutionContextUnavailable = errors.New("interactive task execution context is unavailable")

type rawProcessLaunchSpec struct {
	ProjectID        uuid.UUID
	ProjectRoot      string
	WorkingDirectory string
	Argv             []string
	Environment      []string
	// InteractiveStdin requests a pipe that the caller can write to after
	// launch. It is reserved for bounded device-local protocol probes (for
	// example Codex app-server model discovery); normal task prompts continue
	// to use PrivateStdinPath.
	InteractiveStdin bool
	// PrivateStdinPath is a service-owned, access-controlled file whose open
	// contents are pumped to the child's stdin. It lets a process launched
	// with an interactive user's token consume a task prompt without granting
	// that user access to the Agent's state directory.
	PrivateStdinPath string
	// IgnoreConcurrencyLimit is reserved for the task scheduler. Task
	// concurrency is governed by serial/dependency semantics rather than the
	// generic background-process safety pool.
	IgnoreConcurrencyLimit bool
	Limits                 processResourceLimits
}

type rawProcessHandle interface {
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() int
	Pid() int
	Close() error
}

type rawProcessStarter interface {
	Start(rawProcessLaunchSpec) (rawProcessHandle, error)
}

type execRawProcessStarter struct{}

func (execRawProcessStarter) Start(spec rawProcessLaunchSpec) (rawProcessHandle, error) {
	if len(spec.Argv) == 0 {
		return nil, errRPCInvalid
	}
	command, err := taskExecCommand(spec.Argv[0], spec.Argv[1:])
	if err != nil {
		return nil, fmt.Errorf("prepare raw process: %w", err)
	}
	command.Dir = spec.WorkingDirectory
	command.Env = rawProcessEnvironment(spec.Environment, command.Env)
	var privateInput []byte
	var interactiveStdin io.WriteCloser
	if spec.PrivateStdinPath != "" {
		var inputErr error
		privateInput, inputErr = readPrivateTaskInput(spec.PrivateStdinPath)
		if inputErr != nil {
			return nil, fmt.Errorf("read private process input: %w", inputErr)
		}
		// os/exec connects an *os.File directly as the child's standard
		// handle on Windows. cmd.exe receives that handle, but its npm-style
		// shim descendants do not reliably inherit it and therefore observe
		// immediate EOF. A non-file reader makes os/exec pump the bounded
		// private input through a pipe, which remains usable by descendants.
		command.Stdin = bytes.NewReader(privateInput)
	} else if spec.InteractiveStdin {
		interactiveStdin, err = command.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("create raw stdin pipe: %w", err)
		}
	}
	inputTransferred := false
	defer func() {
		if !inputTransferred {
			clear(privateInput)
		}
	}()
	cleanupCommand, err := configureRawProcessCommand(command)
	if err != nil {
		return nil, err
	}
	defer cleanupCommand()
	configureBackgroundProcess(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create raw stdout pipe: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("create raw stderr pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start raw process: %w", err)
	}
	tree, err := attachRawProcessTree(command)
	if err != nil {
		// A background process is never allowed to run outside the controlled
		// process-tree mechanism.  It is safer to fail the launch than to leave
		// a child process behind after cancellation or Agent shutdown.
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, fmt.Errorf("attach raw process tree: %w", err)
	}
	inputTransferred = true
	return &execRawProcess{
		command: command, stdout: stdout, stderr: stderr, stdin: interactiveStdin, tree: tree,
		privateInput: privateInput, done: make(chan struct{}),
	}, nil
}

func rawProcessEnvironment(reviewed, commandEnvironment []string) []string {
	result := append([]string(nil), reviewed...)
	for _, variable := range commandEnvironment {
		name, _, found := strings.Cut(variable, "=")
		if !found || !strings.HasPrefix(strings.ToUpper(name), "WENZWORK_TASK_EXEC_ARG_") {
			continue
		}
		result = append(result, variable)
	}
	return result
}

// mergeInteractiveCommandEnvironment combines the current interactive user's
// safe process-discovery values with the Agent-reviewed environment. The
// interactive account must remain the source of PATH and profile locations:
// those are where its CLI shims, Node runtime, and CLI login state live. The
// reviewed entries are added for task-specific values, while protected runtime
// keys keep the trusted account values and arbitrary variables (including
// secrets) are never copied.
func mergeInteractiveCommandEnvironment(interactive, reviewed []string) []string {
	type environmentEntry struct {
		name  string
		value string
	}
	entries := make(map[string]environmentEntry, len(interactive)+len(reviewed))
	for _, variable := range interactive {
		name, value, found := strings.Cut(variable, "=")
		if !found || name == "" || !interactiveCommandEnvironmentKey(name) || strings.IndexByte(value, 0) >= 0 {
			continue
		}
		entries[strings.ToUpper(name)] = environmentEntry{name: name, value: value}
	}
	for _, variable := range reviewed {
		name, value, found := strings.Cut(variable, "=")
		if !found || name == "" || strings.IndexByte(value, 0) >= 0 {
			continue
		}
		key := strings.ToUpper(name)
		if interactiveCommandEnvironmentKey(key) {
			if _, exists := entries[key]; exists {
				continue
			}
		}
		entries[key] = environmentEntry{name: name, value: value}
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		entry := entries[key]
		result = append(result, entry.name+"="+entry.value)
	}
	return result
}

func filterCommandRuntimeEnvironment(values []string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		name, contents, found := strings.Cut(value, "=")
		if !found || !interactiveCommandEnvironmentKey(name) || strings.IndexByte(contents, 0) >= 0 {
			continue
		}
		filtered = append(filtered, value)
	}
	return filtered
}

func interactiveCommandEnvironmentKey(name string) bool {
	switch strings.ToUpper(name) {
	case "PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC",
		"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA", "TMP", "TEMP",
		"SHELL", "TERM", "LANG", "LC_ALL", "LC_CTYPE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "SSH_AUTH_SOCK",
		"NVM_HOME", "NVM_SYMLINK", "NVM_BIN", "NVM_DIR", "VOLTA_HOME", "BUN_INSTALL", "PNPM_HOME", "MISE_DATA_DIR", "ASDF_DIR":
		return true
	default:
		return false
	}
}

// rawProcessTree is deliberately small. Platform implementations use a Job
// Object on Windows and a process group on Unix; neither implementation ever
// constructs a taskkill command line.
type rawProcessTree interface {
	Close() error
}

type execRawProcess struct {
	command *exec.Cmd
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	stdin   io.WriteCloser
	tree    rawProcessTree
	// privateInput backs command.Stdin until os/exec's pipe copier completes.
	// It is cleared immediately after Wait so prompt bytes are not retained for
	// the lifetime of the supervisor.
	privateInput []byte

	waitOnce  sync.Once
	closeOnce sync.Once
	done      chan struct{}
	exitCode  int
	closeErr  error
}

func (process *execRawProcess) Stdout() io.ReadCloser {
	if process == nil {
		return nil
	}
	return process.stdout
}

func (process *execRawProcess) Stderr() io.ReadCloser {
	if process == nil {
		return nil
	}
	return process.stderr
}

func (process *execRawProcess) Stdin() io.WriteCloser {
	if process == nil {
		return nil
	}
	return process.stdin
}

func (process *execRawProcess) Wait() int {
	if process == nil || process.command == nil {
		return -1
	}
	process.waitOnce.Do(func() {
		waitErr := process.command.Wait()
		clear(process.privateInput)
		process.privateInput = nil
		process.exitCode = rawProcessExitCode(waitErr)
		close(process.done)
	})
	<-process.done
	return process.exitCode
}

func rawProcessExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

func (process *execRawProcess) Pid() int {
	if process == nil || process.command == nil || process.command.Process == nil {
		return 0
	}
	return process.command.Process.Pid
}

func (process *execRawProcess) Close() error {
	if process == nil {
		return nil
	}
	process.closeOnce.Do(func() {
		if process.tree != nil {
			process.closeErr = process.tree.Close()
			// The platform tree owner is responsible for the direct child too:
			// closing a Windows Job Object or killing a Unix process group already
			// terminates it.  Calling Process.Kill afterwards can turn a successful
			// tree cleanup into a spurious access/process-done error on Windows.
			return
		}
		if process.command != nil && process.command.Process != nil {
			if err := process.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				process.closeErr = errors.Join(process.closeErr, err)
			}
		}
	})
	return process.closeErr
}

type rawProcessSupervisor struct {
	mu                  sync.Mutex
	starter             rawProcessStarter
	memoryBytes         func(int) (uint64, error)
	memoryPollInterval  time.Duration
	maximumConcurrent   int
	environmentProvider func() []string
	processes           map[uuid.UUID]*rawSupervisedProcess
	closed              bool
}

type rawSupervisedProcess struct {
	id         uuid.UUID
	projectID  uuid.UUID
	process    rawProcessHandle
	limits     processResourceLimits
	supervisor *rawProcessSupervisor

	outputBytes atomic.Uint64

	stdout io.ReadCloser
	stderr io.ReadCloser

	mu          sync.Mutex
	closed      bool
	closeReason string
	releaseOnce sync.Once
	done        chan struct{}
}

func newRawProcessSupervisor(environmentProvider ...func() []string) *rawProcessSupervisor {
	supervisor := newRawProcessSupervisorWithDependencies(execRawProcessStarter{}, platformProcessTreeMemoryBytes, defaultMaximumRawProcesses)
	if len(environmentProvider) > 0 {
		supervisor.environmentProvider = environmentProvider[0]
	}
	return supervisor
}

func newRawProcessSupervisorWithDependencies(starter rawProcessStarter, memoryBytes func(int) (uint64, error), maximumConcurrent int) *rawProcessSupervisor {
	if memoryBytes == nil {
		memoryBytes = func(int) (uint64, error) { return 0, errProcessMemoryUnavailable }
	}
	return &rawProcessSupervisor{
		starter: starter, memoryBytes: memoryBytes, memoryPollInterval: processMemoryPollInterval,
		maximumConcurrent: maximumConcurrent, processes: make(map[uuid.UUID]*rawSupervisedProcess),
	}
}

func (supervisor *rawProcessSupervisor) Start(spec rawProcessLaunchSpec) (*rawSupervisedProcess, error) {
	injections := spec.Environment
	if supervisor != nil && supervisor.environmentProvider != nil {
		injections = append(append([]string(nil), supervisor.environmentProvider()...), injections...)
	}
	environment, err := reviewedProcessEnvironment(injections)
	if err != nil {
		return nil, err
	}
	spec.Environment = environment
	if supervisor == nil || supervisor.starter == nil || supervisor.maximumConcurrent < 1 || !validRawProcessLaunchSpec(spec) {
		return nil, errRPCInvalid
	}
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return nil, errRPCCapability
	}
	if !spec.IgnoreConcurrencyLimit && len(supervisor.processes) >= supervisor.maximumConcurrent {
		supervisor.mu.Unlock()
		return nil, errRPCBusy
	}
	processID := uuid.New()
	supervisor.processes[processID] = nil
	supervisor.mu.Unlock()

	handle, err := supervisor.starter.Start(spec)
	if err != nil {
		supervisor.mu.Lock()
		delete(supervisor.processes, processID)
		supervisor.mu.Unlock()
		return nil, fmt.Errorf("start supervised raw process: %w", err)
	}
	if handle == nil || handle.Stdout() == nil || handle.Stderr() == nil {
		if handle != nil {
			_ = handle.Close()
		}
		supervisor.mu.Lock()
		delete(supervisor.processes, processID)
		supervisor.mu.Unlock()
		return nil, errRPCCapability
	}
	process := &rawSupervisedProcess{
		id: processID, projectID: spec.ProjectID, process: handle, limits: spec.Limits, supervisor: supervisor,
		done: make(chan struct{}),
	}
	process.stdout = &rawProcessStream{process: process, source: handle.Stdout()}
	process.stderr = &rawProcessStream{process: process, source: handle.Stderr()}
	supervisor.mu.Lock()
	if supervisor.closed {
		delete(supervisor.processes, processID)
		supervisor.mu.Unlock()
		_ = handle.Close()
		return nil, errRPCCapability
	}
	supervisor.processes[processID] = process
	supervisor.mu.Unlock()
	go supervisor.monitor(process)
	return process, nil
}

func validRawProcessLaunchSpec(spec rawProcessLaunchSpec) bool {
	if spec.ProjectID == uuid.Nil || len(spec.Argv) == 0 || spec.Limits.MaximumLifetime <= 0 ||
		spec.Limits.MaximumMemoryBytes == 0 || spec.Limits.MaximumOutputBytes == 0 {
		return false
	}
	root, rootErr := filepath.Abs(filepath.Clean(spec.ProjectRoot))
	directory, directoryErr := filepath.Abs(filepath.Clean(spec.WorkingDirectory))
	if rootErr != nil || directoryErr != nil || !pathWithinRoot(root, directory) {
		return false
	}
	for _, argument := range spec.Argv {
		if argument == "" || strings.IndexByte(argument, 0) >= 0 {
			return false
		}
	}
	for _, variable := range spec.Environment {
		if strings.IndexByte(variable, 0) >= 0 || !strings.Contains(variable, "=") {
			return false
		}
	}
	if spec.InteractiveStdin && spec.PrivateStdinPath != "" {
		return false
	}
	if spec.PrivateStdinPath != "" && (!filepath.IsAbs(spec.PrivateStdinPath) || strings.IndexByte(spec.PrivateStdinPath, 0) >= 0) {
		return false
	}
	return true
}

func (supervisor *rawProcessSupervisor) monitor(process *rawSupervisedProcess) {
	lifetime := time.NewTimer(process.limits.MaximumLifetime)
	defer lifetime.Stop()
	interval := supervisor.memoryPollInterval
	if interval <= 0 {
		interval = processMemoryPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-process.done:
			return
		case <-lifetime.C:
			_ = process.Close("lifetime_limit")
			return
		case <-ticker.C:
			memory, err := supervisor.memoryBytes(process.Pid())
			if err == nil && memory > process.limits.MaximumMemoryBytes {
				_ = process.Close("memory_limit")
				return
			}
		}
	}
}

type rawProcessStream struct {
	process *rawSupervisedProcess
	source  io.ReadCloser
}

func (stream *rawProcessStream) Read(buffer []byte) (int, error) {
	if stream == nil || stream.source == nil || stream.process == nil {
		return 0, io.EOF
	}
	n, err := stream.source.Read(buffer)
	if n > 0 {
		total := stream.process.outputBytes.Add(uint64(n))
		if total > stream.process.limits.MaximumOutputBytes {
			go func() { _ = stream.process.Close("output_limit") }()
		}
	}
	return n, err
}

func (stream *rawProcessStream) Close() error {
	if stream == nil || stream.source == nil {
		return nil
	}
	return stream.source.Close()
}

func (process *rawSupervisedProcess) Stdout() io.ReadCloser {
	if process == nil {
		return nil
	}
	return process.stdout
}

func (process *rawSupervisedProcess) Stderr() io.ReadCloser {
	if process == nil {
		return nil
	}
	return process.stderr
}

// Stdin is available only for launch specs that explicitly request an
// interactive pipe. Keeping it optional preserves the non-interactive raw
// process contract used by task execution and existing test fakes.
func (process *rawSupervisedProcess) Stdin() io.WriteCloser {
	if process == nil || process.process == nil {
		return nil
	}
	if interactive, ok := process.process.(interface{ Stdin() io.WriteCloser }); ok {
		return interactive.Stdin()
	}
	return nil
}

func (process *rawSupervisedProcess) Wait() int {
	if process == nil || process.process == nil {
		return -1
	}
	return process.process.Wait()
}

func (process *rawSupervisedProcess) Pid() int {
	if process == nil || process.process == nil {
		return 0
	}
	return process.process.Pid()
}

func (process *rawSupervisedProcess) Close(reason string) error {
	if process == nil || process.process == nil {
		return nil
	}
	process.mu.Lock()
	if process.closeReason == "" {
		process.closeReason = reason
	}
	if process.closed {
		process.mu.Unlock()
		return nil
	}
	process.closed = true
	process.mu.Unlock()
	return process.process.Close()
}

func (process *rawSupervisedProcess) reason() string {
	if process == nil {
		return ""
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.closeReason
}

func (process *rawSupervisedProcess) release() {
	if process == nil {
		return
	}
	process.releaseOnce.Do(func() {
		_ = process.Close("process_release")
		close(process.done)
		if process.supervisor != nil {
			process.supervisor.mu.Lock()
			delete(process.supervisor.processes, process.id)
			process.supervisor.mu.Unlock()
		}
	})
}

func (supervisor *rawProcessSupervisor) Close() error {
	if supervisor == nil {
		return nil
	}
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return nil
	}
	supervisor.closed = true
	processes := make([]*rawSupervisedProcess, 0, len(supervisor.processes))
	for _, process := range supervisor.processes {
		if process != nil {
			processes = append(processes, process)
		}
	}
	supervisor.mu.Unlock()
	var result error
	for _, process := range processes {
		result = errors.Join(result, process.Close("agent_exit"))
		process.release()
	}
	return result
}
