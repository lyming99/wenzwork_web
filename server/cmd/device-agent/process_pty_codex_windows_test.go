//go:build windows

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsPTYNestedClientEnvironment = "WENZWORK_TEST_PTY_NESTED_CLIENT"
	windowsPTYNestedClientMarker      = "WENZWORK_NESTED_CONPTY_OK"
)

func TestWindowsSupervisedPTYKeepsNestedConsoleClientInline(t *testing.T) {
	switch os.Getenv(windowsPTYNestedClientEnvironment) {
	case "launcher":
		command := exec.Command(os.Args[0], "-test.run=^TestWindowsSupervisedPTYKeepsNestedConsoleClientInline$")
		command.Env = append(os.Environ(), windowsPTYNestedClientEnvironment+"=client")
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "client":
		for name, descriptor := range map[string]uint32{
			"stdin": windows.STD_INPUT_HANDLE, "stdout": windows.STD_OUTPUT_HANDLE, "stderr": windows.STD_ERROR_HANDLE,
		} {
			handle, err := windows.GetStdHandle(descriptor)
			if err != nil || handle == 0 || handle == windows.InvalidHandle {
				fmt.Fprintf(os.Stderr, "%s has no console handle: handle=%#x err=%v\n", name, handle, err)
				os.Exit(1)
			}
			var mode uint32
			if err := windows.GetConsoleMode(handle, &mode); err != nil {
				fmt.Fprintf(os.Stderr, "%s is not attached to the inherited console: %v\n", name, err)
				os.Exit(1)
			}
		}
		fmt.Println(windowsPTYNestedClientMarker)
		os.Exit(0)
	}

	for _, shell := range []string{"pwsh", "powershell"} {
		executable, err := exec.LookPath(shell + ".exe")
		if err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			script := "& '" + strings.ReplaceAll(os.Args[0], "'", "''") + "' '-test.run=^TestWindowsSupervisedPTYKeepsNestedConsoleClientInline$'; exit $LASTEXITCODE"
			config := windowsPTYTestConfig([]string{executable, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script})
			config.Dir = t.TempDir()
			environment, err := reviewedProcessEnvironment(nil)
			if err != nil {
				t.Fatal(err)
			}
			config.Env = append(
				environment,
				windowsPTYNestedClientEnvironment+"=launcher",
			)
			pty, err := startSupervisedPTY(config)
			if err != nil {
				t.Fatal(err)
			}
			defer pty.Close()
			output, readErr := io.ReadAll(pty)
			exitCode := pty.Wait()
			if readErr != nil {
				t.Fatalf("read nested ConPTY output: %v", readErr)
			}
			if exitCode != 0 || !bytes.Contains(output, []byte(windowsPTYNestedClientMarker)) {
				t.Fatalf("nested ConPTY output=%q exitCode=%d", output, exitCode)
			}
		})
	}

	if _, err := exec.LookPath("pwsh.exe"); err != nil {
		if _, err := exec.LookPath("powershell.exe"); err != nil {
			t.Skip("PowerShell is unavailable")
		}
	}
}

func TestRealCodexRunsInsideWindowsSupervisedPTY(t *testing.T) {
	if os.Getenv("WENZWORK_REAL_CODEX_TEST") != "1" {
		t.Skip("set WENZWORK_REAL_CODEX_TEST=1 to exercise the installed Codex CLI")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("Codex CLI is unavailable")
	}
	shell, executable := firstAvailableWindowsPowerShell(t)
	config := windowsPTYTestConfig(append([]string{executable}, terminalShellArgumentsForOS(shell, "windows")...))
	config.Dir = t.TempDir()
	environment, err := reviewedProcessEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	config.Env = environment
	pty, err := startSupervisedPTY(config)
	if err != nil {
		t.Fatal(err)
	}

	output := newSynchronizedBuffer()
	readDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(output, pty)
		close(readDone)
	}()
	if !waitForSynchronizedBuffer(output, []byte(">"), 10*time.Second) {
		_ = pty.Close()
		<-readDone
		t.Fatalf("PowerShell prompt did not start inside ConPTY: %q", output.String())
	}
	if _, err := pty.Write([]byte("codex\r")); err != nil {
		t.Fatal(err)
	}
	if !waitForSynchronizedBufferAny(
		output,
		[][]byte{
			[]byte("OpenAI Codex"),
			[]byte("Update available!"),
			[]byte("Welcome to Codex"),
			[]byte("Sign in with ChatGPT"),
		},
		20*time.Second,
	) {
		_ = pty.Close()
		<-readDone
		t.Fatalf("Codex TUI did not render through the current ConPTY: %q", output.String())
	}
	_ = pty.Close()
	<-readDone
}

func firstAvailableWindowsPowerShell(t *testing.T) (string, string) {
	t.Helper()
	for _, shell := range []string{"pwsh", "powershell"} {
		if executable, err := exec.LookPath(shell + ".exe"); err == nil {
			return shell, executable
		}
	}
	t.Skip("PowerShell is unavailable")
	return "", ""
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func newSynchronizedBuffer() *synchronizedBuffer {
	return &synchronizedBuffer{}
}

func (buffer *synchronizedBuffer) Write(contents []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(contents)
}

func (buffer *synchronizedBuffer) Bytes() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return append([]byte(nil), buffer.buffer.Bytes()...)
}

func (buffer *synchronizedBuffer) String() string {
	return string(buffer.Bytes())
}

func waitForSynchronizedBuffer(buffer *synchronizedBuffer, marker []byte, timeout time.Duration) bool {
	return waitForSynchronizedBufferAny(buffer, [][]byte{marker}, timeout)
}

func waitForSynchronizedBufferAny(buffer *synchronizedBuffer, markers [][]byte, timeout time.Duration) bool {
	containsMarker := func() bool {
		contents := buffer.Bytes()
		for _, marker := range markers {
			if bytes.Contains(contents, marker) {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if containsMarker() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return containsMarker()
}
