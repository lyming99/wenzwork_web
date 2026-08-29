package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kodecable/crosspty"
	"github.com/google/uuid"
)

const (
	defaultMaximumSupervisedProcesses = 8
	processMemoryPollInterval         = 2 * time.Second
)

// processPTY is the small cross-platform seam used by ProcessSupervisor.
// Production uses CrossPTY (Unix PTY or Windows ConPTY); tests inject a fake
// without starting a real shell.
type processPTY interface {
	io.Reader
	io.Writer
	Close() error
	Wait() int
	Pid() int
	Resize(crosspty.TermSize) error
}

type processPTYStarter interface {
	Start(processLaunchSpec) (processPTY, error)
}

type crossPTYStarter struct{}

func (crossPTYStarter) Start(spec processLaunchSpec) (processPTY, error) {
	return startSupervisedPTY(crosspty.CommandConfig{
		Argv: spec.Argv,
		Dir:  spec.WorkingDirectory,
		// An explicit empty/allowlisted environment is important: CrossPTY's
		// zero value otherwise inherits the full Agent service environment.
		Env: spec.Environment,
		EnvFallback: map[string]string{
			"TERM": "xterm-256color",
		},
		EnvInject: map[string]string{
			"TERM": "xterm-256color",
		},
		Size: crosspty.TermSize{Rows: spec.Rows, Cols: spec.Columns},
		CloseConfig: crosspty.CloseConfig{
			CloseTimeout: 4 * time.Second,
			KillDelay:    2 * time.Second,
			KillExitCode: 137,
			KillMode:     crosspty.KillModeKillGroupOnSubProcessExit,
		},
	})
}

type processResourceLimits struct {
	MaximumLifetime    time.Duration
	MaximumMemoryBytes uint64
	MaximumOutputBytes uint64
}

type processLaunchSpec struct {
	ProjectID        uuid.UUID
	ProjectRoot      string
	WorkingDirectory string
	Argv             []string
	Environment      []string
	// InheritHostEnvironment is reserved for a user-opened interactive
	// terminal. Background/AI PTYs keep the reviewed minimal environment so
	// ambient service credentials are not exposed to an automated command.
	InheritHostEnvironment bool
	Rows                   uint16
	Columns                uint16
	Limits                 processResourceLimits
}

type processSupervisor struct {
	mu                  sync.Mutex
	starter             processPTYStarter
	memoryBytes         func(int) (uint64, error)
	memoryPollInterval  time.Duration
	maximumConcurrent   int
	environmentProvider func() []string
	hostEnvironment     func() []string
	processes           map[uuid.UUID]*supervisedProcess
	closed              bool
}

type supervisedProcess struct {
	id         uuid.UUID
	projectID  uuid.UUID
	pty        processPTY
	limits     processResourceLimits
	supervisor *processSupervisor

	outputBytes atomic.Uint64
	inputBytes  atomic.Uint64

	mu          sync.Mutex
	closed      bool
	closeReason string
	releaseOnce sync.Once
	done        chan struct{}
}

func newProcessSupervisor(environmentProvider ...func() []string) *processSupervisor {
	supervisor := newProcessSupervisorWithDependencies(crossPTYStarter{}, platformProcessTreeMemoryBytes, defaultMaximumSupervisedProcesses)
	if len(environmentProvider) > 0 {
		supervisor.environmentProvider = environmentProvider[0]
	}
	if len(environmentProvider) > 1 {
		supervisor.hostEnvironment = environmentProvider[1]
	}
	return supervisor
}

func newProcessSupervisorWithDependencies(starter processPTYStarter, memoryBytes func(int) (uint64, error), maximumConcurrent int) *processSupervisor {
	if memoryBytes == nil {
		memoryBytes = func(int) (uint64, error) { return 0, errProcessMemoryUnavailable }
	}
	return &processSupervisor{
		starter: starter, memoryBytes: memoryBytes, memoryPollInterval: processMemoryPollInterval,
		maximumConcurrent: maximumConcurrent,
		processes:         make(map[uuid.UUID]*supervisedProcess),
	}
}

func (supervisor *processSupervisor) Start(spec processLaunchSpec) (*supervisedProcess, error) {
	// Automated PTY callers never inherit the Agent service environment. A
	// user-opened terminal starts from the host snapshot captured before
	// agent.env is loaded, so ordinary system variables remain available while
	// service configuration and credentials cannot leak into the shell.
	injections := spec.Environment
	if supervisor != nil && supervisor.environmentProvider != nil {
		injections = append(append([]string(nil), supervisor.environmentProvider()...), injections...)
	}
	base := minimalTerminalEnvironment()
	if spec.InheritHostEnvironment && supervisor != nil && supervisor.hostEnvironment != nil {
		base = supervisor.hostEnvironment()
	}
	environment, err := reviewedProcessEnvironmentWithBase(base, injections, spec.InheritHostEnvironment)
	if err != nil {
		return nil, err
	}
	spec.Environment = environment
	if supervisor == nil || supervisor.starter == nil || supervisor.maximumConcurrent < 1 || !validProcessLaunchSpec(spec) {
		return nil, errRPCInvalid
	}
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return nil, errRPCCapability
	}
	if len(supervisor.processes) >= supervisor.maximumConcurrent {
		supervisor.mu.Unlock()
		return nil, errRPCBusy
	}
	processID := uuid.New()
	// Reserve before starting so concurrent opens cannot pass the limit.
	supervisor.processes[processID] = nil
	supervisor.mu.Unlock()

	pty, err := supervisor.starter.Start(spec)
	if err != nil {
		supervisor.mu.Lock()
		delete(supervisor.processes, processID)
		supervisor.mu.Unlock()
		return nil, fmt.Errorf("start supervised PTY: %w", err)
	}
	process := &supervisedProcess{
		id: processID, projectID: spec.ProjectID, pty: pty, limits: spec.Limits,
		supervisor: supervisor, done: make(chan struct{}),
	}
	supervisor.mu.Lock()
	if supervisor.closed {
		delete(supervisor.processes, processID)
		supervisor.mu.Unlock()
		_ = pty.Close()
		return nil, errRPCCapability
	}
	supervisor.processes[processID] = process
	supervisor.mu.Unlock()
	go supervisor.monitor(process)
	return process, nil
}

func reviewedProcessEnvironment(injections []string) ([]string, error) {
	return reviewedProcessEnvironmentWithBase(minimalTerminalEnvironment(), injections, false)
}

func reviewedProcessEnvironmentWithBase(base, injections []string, omitAgentPrivate bool) ([]string, error) {
	type environmentEntry struct {
		name  string
		value string
	}
	entries := make(map[string]environmentEntry)
	for _, variable := range base {
		name, value, found := strings.Cut(variable, "=")
		if !found || name == "" || strings.IndexByte(value, 0) >= 0 {
			return nil, errRPCInvalid
		}
		upper := strings.ToUpper(name)
		if omitAgentPrivate && strings.HasPrefix(upper, "WENZWORK_") {
			continue
		}
		entries[upper] = environmentEntry{name: name, value: value}
	}
	for _, variable := range injections {
		name, value, found := strings.Cut(variable, "=")
		upper := strings.ToUpper(name)
		if !found || !taskEnvironmentNamePattern.MatchString(name) || strings.IndexByte(value, 0) >= 0 ||
			len(value) > 8<<10 {
			return nil, errRPCInvalid
		}
		if protectedProcessEnvironmentName(upper) {
			return nil, errRPCInvalid
		}
		entries[upper] = environmentEntry{name: name, value: value}
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
	return result, nil
}

func validProcessLaunchSpec(spec processLaunchSpec) bool {
	if spec.ProjectID == uuid.Nil || len(spec.Argv) == 0 || spec.Rows < 2 || spec.Columns < 10 ||
		spec.Limits.MaximumLifetime < 0 || spec.Limits.MaximumMemoryBytes == 0 || spec.Limits.MaximumOutputBytes == 0 {
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
	return true
}

func pathWithinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (supervisor *processSupervisor) monitor(process *supervisedProcess) {
	var lifetime <-chan time.Time
	var lifetimeTimer *time.Timer
	if process.limits.MaximumLifetime > 0 {
		lifetimeTimer = time.NewTimer(process.limits.MaximumLifetime)
		lifetime = lifetimeTimer.C
		defer lifetimeTimer.Stop()
	}
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
		case <-lifetime:
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

func (process *supervisedProcess) Read(buffer []byte) (int, error) {
	if process == nil || process.pty == nil {
		return 0, io.EOF
	}
	n, err := process.pty.Read(buffer)
	if n > 0 {
		total := process.outputBytes.Add(uint64(n))
		if total > process.limits.MaximumOutputBytes {
			go func() { _ = process.Close("output_limit") }()
		}
	}
	return n, err
}

func (process *supervisedProcess) Write(buffer []byte) (int, error) {
	if process == nil || process.pty == nil || process.isClosed() {
		return 0, io.ErrClosedPipe
	}
	n, err := process.pty.Write(buffer)
	if n > 0 {
		process.inputBytes.Add(uint64(n))
	}
	return n, err
}

func (process *supervisedProcess) Resize(rows, columns uint16) error {
	if process == nil || process.pty == nil || process.isClosed() {
		return io.ErrClosedPipe
	}
	return process.pty.Resize(crosspty.TermSize{Rows: rows, Cols: columns})
}

func (process *supervisedProcess) Wait() int {
	if process == nil || process.pty == nil {
		return -1
	}
	return process.pty.Wait()
}

func (process *supervisedProcess) Pid() int {
	if process == nil || process.pty == nil {
		return 0
	}
	return process.pty.Pid()
}

func (process *supervisedProcess) Close(reason string) error {
	if process == nil || process.pty == nil {
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
	return process.pty.Close()
}

func (process *supervisedProcess) isClosed() bool {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.closed
}

func (process *supervisedProcess) reason() string {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.closeReason
}

func (process *supervisedProcess) release() {
	if process == nil {
		return
	}
	process.releaseOnce.Do(func() {
		close(process.done)
		if process.supervisor != nil {
			process.supervisor.mu.Lock()
			delete(process.supervisor.processes, process.id)
			process.supervisor.mu.Unlock()
		}
	})
}

func (supervisor *processSupervisor) Close() error {
	if supervisor == nil {
		return nil
	}
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return nil
	}
	supervisor.closed = true
	processes := make([]*supervisedProcess, 0, len(supervisor.processes))
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

func resolveSupervisedExecutable(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\\`) {
		return "", errRPCInvalid
	}
	resolved, err := lookupCommandExecutable(name)
	if err != nil {
		return "", errRPCCapability
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errRPCCapability
	}
	return resolved, nil
}
